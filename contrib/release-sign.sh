#!/bin/sh
# Sign and notarize the macOS binaries of an existing muster release, then
# replace the release's darwin assets (and checksums) with the signed ones.
# Covers BOTH shipped binaries: muster, and muster-deploy for the optional
# hosted backend.
# CI attaches unsigned binaries; run this once per release from a Mac with a
# Developer ID certificate in the keychain and a stored notarytool profile:
#
#   contrib/release-sign.sh v0.3.1
#
# Knobs (env): MUSTER_SIGN_IDENTITY (default: first "Developer ID Application"
# identity in the keychain), MUSTER_NOTARY_PROFILE (default: muster-notary).
set -eu
tag="${1:?usage: release-sign.sh <tag>}"
repo="${MUSTER_REPO_SLUG:-subaud-tools/muster}"
profile="${MUSTER_NOTARY_PROFILE:-muster-notary}"
identity="${MUSTER_SIGN_IDENTITY:-$(security find-identity -v -p codesigning \
  | sed -n 's/.*"\(Developer ID Application:[^"]*\)".*/\1/p' | head -1)}"
[ -n "$identity" ] || { echo "no Developer ID Application identity found" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
echo "signing as: $identity"

# rebuild the darwin binaries from the tag so we sign exactly the released code
src="$work/src"
git clone --quiet --depth 1 --branch "$tag" "https://github.com/$repo" "$src"
# stamp version/commit exactly as release.yml does, so signed binaries don't
# regress `muster version` to "dev (none)"
version="${tag#v}"
commit="$(cd "$src" && git rev-parse --short HEAD)"
ldflags="-s -w -X github.com/subaud-tools/muster/internal/version.version=${version} -X github.com/subaud-tools/muster/internal/version.commit=${commit}"
# Every darwin binary the release ships has to be signed. An unsigned one is
# quarantined by Gatekeeper on download and refuses to run, which looks like a
# broken build rather than a missing signature.
for arch in arm64 amd64; do
  for bin in muster muster-deploy; do
    dir="$work/${bin}_darwin_${arch}"
    mkdir -p "$dir"
    (cd "$src" && CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
      go build -trimpath -ldflags "$ldflags" -o "$dir/$bin" "./cmd/$bin")
    codesign --sign "$identity" --options runtime --timestamp --force "$dir/$bin"
    ditto -c -k --keepParent "$dir/$bin" "$work/notarize_${bin}_${arch}.zip"
    xcrun notarytool submit "$work/notarize_${bin}_${arch}.zip" \
      --keychain-profile "$profile" --wait | grep -E "status:" | tail -1
    tar -C "$dir" -czf "$work/${bin}_darwin_${arch}.tar.gz" "$bin"
  done
done

# Recompute checksums across the FULL asset set — signed darwin, CI's linux,
# and the Lambda zip for the hosted backend (from muster.tools/dl, where the
# release publishes it). This file replaces CI's, so every
# asset CI checksummed has to be downloaded and re-hashed here: anything left
# out silently vanishes from checksums.txt and looks like an unverifiable
# download to whoever fetches it. The zip is a linux/arm64 binary and is not
# signed or notarized — nothing on macOS ever executes it.
#
# Note the globs: muster_* does NOT match muster-deploy_* (underscore versus
# hyphen), so the deploy tarballs need naming separately. Miss them and
# install.sh --with-deploy fails outright with "no checksum found".
(cd "$work" && \
  gh release download "$tag" --repo "$repo" \
    --pattern "muster_linux_*.tar.gz" --pattern "muster-deploy_linux_*.tar.gz" && \
  curl -fsSLO "https://muster.tools/dl/muster/$version/muster-lambda-arm64-$tag.zip" && \
  shasum -a 256 muster_*.tar.gz muster-deploy_*.tar.gz muster-lambda-*.zip > checksums.txt && \
  gh release upload "$tag" muster_darwin_*.tar.gz muster-deploy_darwin_*.tar.gz \
    checksums.txt --clobber --repo "$repo")
echo "done: darwin assets for $tag are signed + notarized"
