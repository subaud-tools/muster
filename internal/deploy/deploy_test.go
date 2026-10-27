package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"

	cfn "github.com/subaud-tools/muster/contrib/cloudformation"
)

func TestReleaseTagAddsVPrefixExactlyOnce(t *testing.T) {
	// The VERSION file carries no "v" and release tags do; this is the one
	// place that converts, so it has to be idempotent — a caller passing an
	// already-tagged string must not get "vv0.11.0" and a 404 an hour later.
	for in, want := range map[string]string{
		"0.11.0": "v0.11.0", "v0.11.0": "v0.11.0", "": "",
	} {
		if got := ReleaseTag(in); got != want {
			t.Errorf("ReleaseTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArtifactNameMatchesReleaseWorkflow(t *testing.T) {
	// This name is a CONTRACT with .github/workflows/release.yml. If the
	// workflow's zipname changes and this does not, every deploy 404s at
	// download time with nothing to explain why.
	if got, want := ArtifactName("v0.11.0"), "muster-lambda-arm64-v0.11.0.zip"; got != want {
		t.Fatalf("ArtifactName = %q, want %q", got, want)
	}
	// go-release publishes extra assets under the bare semver, beside the
	// tarballs; the zip's own name keeps the tag.
	if got, want := ArtifactURL(DLBase, "v0.11.0"),
		"https://muster.tools/dl/muster/0.11.0/muster-lambda-arm64-v0.11.0.zip"; got != want {
		t.Fatalf("ArtifactURL = %q, want %q", got, want)
	}
}

// serveArtifact serves a tag's zip and a .sha256 sidecar holding sum, the
// way go-release lays them out under /dl.
func serveArtifact(t *testing.T, tag string, zip []byte, sum string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zipPath := "/muster/" + strings.TrimPrefix(tag, "v") + "/" + ArtifactName(tag)
		switch r.URL.Path {
		case zipPath:
			_, _ = w.Write(zip)
		case zipPath + ".sha256":
			_, _ = io.WriteString(w, sum+"  "+ArtifactName(tag)+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFetchArtifactChecksItsSHA256(t *testing.T) {
	zip := []byte("lambda zip bytes")
	sum := sha256.Sum256(zip)
	base := serveArtifact(t, "v0.27.4", zip, hex.EncodeToString(sum[:]))

	path, cleanup, err := fetchArtifact(base, "v0.27.4")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got, _ := os.ReadFile(path); string(got) != string(zip) {
		t.Fatalf("fetched %q, want %q", got, zip)
	}
}

func TestFetchArtifactFetchesAGatedReleaseThroughSignedLinks(t *testing.T) {
	zip := []byte("lambda zip bytes")
	sum := sha256.Sum256(zip)
	signed := serveArtifact(t, "v0.27.4", zip, hex.EncodeToString(sum[:]))

	defer func(g bool, l func(context.Context, string, string) (string, error)) { gated, link = g, l }(gated, link)
	gated = true
	var asked []string
	link = func(_ context.Context, product, path string) (string, error) {
		asked = append(asked, product+" "+path)
		return signed + "/muster/" + path + "?sig=x", nil
	}

	path, cleanup, err := fetchArtifact("https://unreachable.invalid", "v0.27.4")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got, _ := os.ReadFile(path); string(got) != string(zip) {
		t.Fatalf("fetched %q, want %q", got, zip)
	}
	want := []string{
		"muster 0.27.4/muster-lambda-arm64-v0.27.4.zip.sha256",
		"muster 0.27.4/muster-lambda-arm64-v0.27.4.zip",
	}
	if strings.Join(asked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("asked for links %q, want %q", asked, want)
	}
}

func TestFetchArtifactRefusesAChecksumMismatch(t *testing.T) {
	base := serveArtifact(t, "v0.27.4", []byte("tampered"), strings.Repeat("0", 64))

	if _, _, err := fetchArtifact(base, "v0.27.4"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
}

func TestBucketNameIsAccountScoped(t *testing.T) {
	// S3 bucket names are globally unique across all of AWS, so a fixed name
	// would collide with every other operator who ran this tool.
	if got, want := BucketName("316091283514"), "muster-artifacts-316091283514"; got != want {
		t.Fatalf("BucketName = %q, want %q", got, want)
	}
}

func TestGenerateTokenHasFullEntropyAndIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("token is not valid base64: %v", err)
		}
		// 32 bytes, because this token is the ONLY thing in front of the bus
		// and the template's 32-CHARACTER floor would be satisfied by far
		// less entropy than that.
		if len(raw) != 32 {
			t.Fatalf("token decodes to %d bytes, want 32", len(raw))
		}
		if len(tok) < 32 {
			t.Fatalf("token %q is shorter than the template's MinLength", tok)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("GenerateToken repeated a value after %d draws", i)
		}
		seen[tok] = struct{}{}
	}
}

func TestWriteTokenIsOwnerOnly(t *testing.T) {
	// The daemon refuses a token file others can read, so writing one that
	// way would produce a stack that deploys and a device that will not
	// start.
	path := filepath.Join(t.TempDir(), "nested", "remote-token")
	if err := WriteToken(path, "sekrit"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %o, want 600", perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "sekrit" {
		t.Fatalf("token file = %q, want %q", b, "sekrit")
	}
}

// TestFingerprintMatchesTheDocumentedShellCommand pins Fingerprint to the
// exact pipeline docs/hosted-backend.md tells operators to run on the other
// machine — `tr -d '\n' | shasum -a 256 | cut -c1-16`. The two must agree or
// the verification step reports a mismatch for a token that copied fine, and
// an operator would rightly trust the shell over the tool.
func TestFingerprintMatchesTheDocumentedShellCommand(t *testing.T) {
	sum := sha256.Sum256([]byte("muster"))
	if want := hex.EncodeToString(sum[:])[:16]; Fingerprint("muster") != want {
		t.Fatalf("Fingerprint = %q, want %q (sha256 hex, first 16)", Fingerprint("muster"), want)
	}
	// Trailing whitespace must not change the answer: the shell side strips
	// newlines with tr, and a token file written by `echo` carries one.
	if Fingerprint("muster") != Fingerprint("muster\n") {
		t.Fatal("Fingerprint differs on a trailing newline; the shell side strips it")
	}
	if len(Fingerprint("muster")) != 16 {
		t.Fatalf("Fingerprint length = %d, want 16", len(Fingerprint("muster")))
	}
}

// TestValidateDomainRejectsTheWedgedStackCombination guards the worst failure
// this template can produce. A domain with no hosted zone and no certificate
// leaves CloudFormation waiting on a DNS validation record nothing will ever
// publish — CREATE_IN_PROGRESS for hours, then a timeout, with no event
// explaining it. Catching it before the first API call is the whole point.
func TestValidateDomainRejectsTheWedgedStackCombination(t *testing.T) {
	for _, tc := range []struct {
		name    string
		o       Options
		wantErr bool
	}{
		{"no domain at all", Options{}, false},
		{"domain with zone", Options{Domain: "m.example.com", HostedZoneID: "Z123"}, false},
		{"domain with cert", Options{Domain: "m.example.com", CertARN: "arn:aws:acm:…"}, false},
		{"domain with neither", Options{Domain: "m.example.com"}, true},
		{"zone without domain", Options{HostedZoneID: "Z123"}, true},
		{"cert without domain", Options{CertARN: "arn:aws:acm:…"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.o.validateDomain(); (err != nil) != tc.wantErr {
				t.Fatalf("validateDomain() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestDomainParamsPreserveUnlessAskedToRemove is a destroy-guard. A routine
// redeploy to ship new function code passes no domain flags; if that sent
// DomainName="" the stack would delete the custom domain, its certificate and
// its DNS record, and every device pointed at that hostname would stop
// resolving. Absent flags must mean "leave it alone" — the same rule the
// bearer token already follows, for the same reason.
func TestDomainParamsPreserveUnlessAskedToRemove(t *testing.T) {
	find := func(ps []cfntypes.Parameter, key string) cfntypes.Parameter {
		t.Helper()
		for _, p := range ps {
			if aws.ToString(p.ParameterKey) == key {
				return p
			}
		}
		t.Fatalf("parameter %q not sent at all", key)
		return cfntypes.Parameter{}
	}

	// Redeploy of an EXISTING stack with no domain flags: keep what is there.
	p := find(domainParams(Options{}, true), "DomainName")
	if !aws.ToBool(p.UsePreviousValue) {
		t.Fatalf("redeploy without -domain sent %q instead of UsePreviousValue — this deletes a live domain",
			aws.ToString(p.ParameterValue))
	}

	// Explicitly removing: clear it.
	p = find(domainParams(Options{RemoveDomain: true}, true), "DomainName")
	if aws.ToBool(p.UsePreviousValue) || aws.ToString(p.ParameterValue) != "" {
		t.Fatal("-remove-domain did not clear DomainName")
	}

	// Setting one: send it.
	p = find(domainParams(Options{Domain: "m.example.com", HostedZoneID: "Z1"}, true), "DomainName")
	if got := aws.ToString(p.ParameterValue); got != "m.example.com" {
		t.Fatalf("DomainName = %q, want m.example.com", got)
	}

	// A CREATE has no previous value to preserve, so it must send a literal.
	p = find(domainParams(Options{}, false), "DomainName")
	if aws.ToBool(p.UsePreviousValue) {
		t.Fatal("CREATE sent UsePreviousValue, which CloudFormation rejects on a new stack")
	}
}

// TestEmbeddedTemplateDeclaresTheParametersWeSet is the drift guard that
// matters. applyStack sets three parameters by name; the template declares
// them. Those are two files that nothing else couples, so renaming a
// parameter in the template compiles fine, passes every other test, and
// fails only against real AWS at deploy time with a message about an
// unrecognised parameter.
func TestEmbeddedTemplateDeclaresTheParametersWeSet(t *testing.T) {
	if len(cfn.Template) == 0 {
		t.Fatal("embedded template is empty — did the go:embed directive break?")
	}
	for _, param := range []string{"CodeS3Bucket", "CodeS3Key", "MusterToken", "DomainName", "HostedZoneId", "CertificateArn"} {
		// Anchored to a two-space indent so this matches the Parameters block
		// rather than any prose or !Ref elsewhere in the file.
		re := regexp.MustCompile(`(?m)^  ` + param + `:$`)
		if !re.MatchString(cfn.Template) {
			t.Errorf("template declares no %q parameter, but applyStack sets it", param)
		}
	}
}

// TestEmbeddedTemplateOutputsTheEndpointWeRead pins the other half of the
// same coupling: Run reads MusterUrl / TableName / FunctionName out of the
// stack, and an output renamed in the template would surface as a blank
// endpoint in the success message rather than as an error.
func TestEmbeddedTemplateOutputsTheEndpointWeRead(t *testing.T) {
	i := strings.Index(cfn.Template, "\nOutputs:")
	if i < 0 {
		t.Fatal("embedded template has no Outputs block at all")
	}
	outputs := cfn.Template[i:]
	for _, key := range []string{"MusterUrl", "TableName", "FunctionName"} {
		if !regexp.MustCompile(`(?m)^  ` + key + `:$`).MatchString(outputs) {
			t.Errorf("template has no %q output, but Run reads it", key)
		}
	}
}
