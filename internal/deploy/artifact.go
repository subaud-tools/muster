package deploy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/subaud-tools/tools-common/auth"

	"github.com/subaud-tools/muster/internal/version"
)

// DLBase is muster.tools' download page, where the release workflow
// publishes the Lambda zip and its .sha256 beside the device tarballs. A
// self-hoster building their own fork builds their own zip and passes -zip.
const DLBase = "https://muster.tools/dl"

// maxArtifactBytes caps a downloaded Lambda zip. The real artifact is ~7MB;
// this bounds what a redirected or hijacked URL can write to disk and to
// memory before anything looks at it.
const maxArtifactBytes = 64 << 20

// ArtifactName is the Lambda zip's filename for a release tag. It matches
// .github/workflows/release.yml exactly — the name is version-stamped
// because CloudFormation only redeploys Lambda code when S3Key (or
// S3ObjectVersion) CHANGES. Overwriting one fixed key and re-running deploy
// leaves the OLD binary running with no error anywhere, so the version lives
// in the key and upgrades are correct by default.
func ArtifactName(tag string) string {
	return "muster-lambda-arm64-" + tag + ".zip"
}

// ArtifactURL is where a tag's Lambda zip is published under base: the
// version directory is the bare semver, the zip's own name keeps the tag.
func ArtifactURL(base, tag string) string {
	return fmt.Sprintf("%s/muster/%s/%s", base, strings.TrimPrefix(tag, "v"), ArtifactName(tag))
}

// gated and link are version.Gated and auth.Link, as variables so a test can
// fetch through a link source of its own.
var (
	gated = version.Gated
	link  = auth.Link
)

// fetchArtifact downloads a tag's Lambda zip from base and checks it against
// the .sha256 published beside it, failing closed: the zip becomes a Lambda
// holding the bus token, so bytes that do not match what the release
// published never reach S3. It returns the zip's path and a cleanup func.
func fetchArtifact(base, tag string) (path string, cleanup func(), err error) {
	url := ArtifactURL(base, tag)
	dir := strings.TrimPrefix(tag, "v") + "/"
	sumPath, sumCleanup, err := fetch(url+".sha256", dir+ArtifactName(tag)+".sha256")
	if err != nil {
		return "", nil, err
	}
	defer sumCleanup()
	sidecar, err := os.ReadFile(sumPath)
	if err != nil {
		return "", nil, err
	}
	want := ""
	if f := strings.Fields(string(sidecar)); len(f) > 0 {
		want = strings.ToLower(f[0])
	}

	path, cleanup, err = fetch(url, dir+ArtifactName(tag))
	if err != nil {
		return "", nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	_ = f.Close()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if want == "" || hex.EncodeToString(h.Sum(nil)) != want {
		cleanup()
		return "", nil, fmt.Errorf("download %s: checksum mismatch against %s.sha256", url, ArtifactName(tag))
	}
	return path, cleanup, nil
}

// fetch downloads url, or for a gated muster.tools the signed link to path
// (relative to /dl/muster/), the way `muster update` fetches.
func fetch(url, path string) (string, func(), error) {
	if !gated {
		return download(url)
	}
	signed, err := link(context.Background(), "muster", path)
	switch {
	case errors.Is(err, auth.ErrNotSignedIn):
		return "", nil, errors.New("this machine is not signed in to subaud.tools: run `muster login`")
	case errors.Is(err, auth.ErrSignInRefused):
		return "", nil, errors.New("subaud.tools refused this machine's sign-in: run `muster login` to sign in again")
	case err != nil:
		return "", nil, err
	}
	return download(signed)
}

// ReleaseTag turns a version stamp into its release tag. Release tags carry a
// leading "v" and the VERSION file does not, so exactly one place converts
// between them.
func ReleaseTag(version string) string {
	if version == "" {
		return ""
	}
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

// BucketName is the default artifact bucket for an account: muster-artifacts-
// plus the account id. S3 bucket names are globally unique across all of AWS,
// so a fixed name would collide with every other operator's; the account id
// is the shortest thing that is both unique to the operator and derivable
// without asking them for one more parameter.
func BucketName(accountID string) string {
	return "muster-artifacts-" + accountID
}

// GenerateToken mints a bearer token: 32 bytes from crypto/rand, base64. This
// is the ONLY thing standing between the internet and the whole bus (the
// endpoint has no authorizer), so it is never derived from a hostname, a
// timestamp, or anything else guessable — entropy is the entire security
// argument, and the template's 32-character minimum is a floor rather than a
// substitute for it.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// WriteToken writes tok to path with mode 0600, creating parent directories.
// The mode is set through the open rather than a following chmod so the file
// is never briefly readable by others — the daemon refuses to start on a
// looser mode, and a window where it is loose is a window where something
// could have read it.
func WriteToken(path, tok string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, tok); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Fingerprint is a short, non-reversible identifier for a token: the first 16
// hex characters of its SHA-256. Two machines can compare fingerprints in the
// clear to confirm they hold the SAME token, which is the only way to verify a
// hand-copied secret without either party revealing it. Truncated because the
// job is catching a typo or a truncated paste, not resisting a preimage
// attack on a value both sides already have.
func Fingerprint(tok string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	return hex.EncodeToString(sum[:])[:16]
}

// download fetches url into a temporary file and returns its path plus a
// cleanup func. It streams to disk rather than into memory: the caller hands
// the result straight to S3, which wants an io.Reader it can seek for the
// checksum anyway.
func download(rawURL string) (path string, cleanup func(), err error) {
	// A signed link's query is its signature: name the file without it.
	url := strings.SplitN(rawURL, "?", 2)[0]
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(rawURL)
	if err != nil {
		var ue *neturl.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// 404 is the overwhelmingly likely failure and it has one cause worth
		// naming, because the fix is not obvious from "404": the tag does not
		// exist, or it was released before muster.tools/dl carried the zip.
		return "", nil, fmt.Errorf("download %s: HTTP %d (does that release exist, and is its Lambda artifact on muster.tools/dl? if not, pass -zip)",
			url, resp.StatusCode)
	}
	f, err := os.CreateTemp("", "muster-lambda-*.zip")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxArtifactBytes))
	if err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("download %s: %w", url, err)
	}
	if n == maxArtifactBytes {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("download %s: artifact exceeds %d bytes", url, int64(maxArtifactBytes))
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}
