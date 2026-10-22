#!/usr/bin/env bash
# One-command TestFlight upload for Home Access.
#   ./upload.sh            bump build number, archive, export, upload
#   ./upload.sh --no-bump  reuse the current build number (e.g. retry a failed upload)
#
# Credentials: the App Store Connect API key identifiers are read from 1Password
# at run time (item "Apple AppStoreConnect Key"), so nothing secret is committed.
# The .p8 is fetched into ~/.appstoreconnect/private_keys the first time if missing.
# Requires: xcodegen, xcodebuild, the 1Password CLI (`op`) unlocked via the app
# integration, and an existing App Store Connect app record for the bundle id.
set -euo pipefail
cd "$(dirname "$0")"

OP_ITEM="${OP_ITEM:-Apple AppStoreConnect Key}"
OP_VAULT="${OP_VAULT:-Personal}"

echo "==> Reading API key identifiers from 1Password ($OP_ITEM)"
KID=$(op item get "$OP_ITEM" --vault "$OP_VAULT" --fields label='Key ID' --reveal)
ISS=$(op item get "$OP_ITEM" --vault "$OP_VAULT" --fields label='Issuer ID' --reveal)
TEAM=$(op item get "$OP_ITEM" --vault "$OP_VAULT" --fields label='Team ID' --reveal)
: "${KID:?could not read Key ID}" "${ISS:?could not read Issuer ID}" "${TEAM:?could not read Team ID}"

KEYDIR="$HOME/.appstoreconnect/private_keys"
P8="$KEYDIR/AuthKey_$KID.p8"
if [[ ! -f "$P8" ]]; then
  echo "==> Fetching .p8 into $KEYDIR"
  mkdir -p "$KEYDIR"; chmod 700 "$KEYDIR"
  op read "op://$OP_VAULT/$OP_ITEM/AuthKey_$KID.p8" --out-file "$P8"
  chmod 600 "$P8"
fi

# Bump CURRENT_PROJECT_VERSION in project.yml (the XcodeGen source of truth) unless --no-bump.
if [[ "${1:-}" != "--no-bump" ]]; then
  CUR=$(grep -E 'CURRENT_PROJECT_VERSION:' project.yml | head -1 | grep -oE '[0-9]+')
  NEXT=$((CUR + 1))
  echo "==> Bumping build number $CUR -> $NEXT (commit project.yml after a successful upload)"
  /usr/bin/sed -i '' -E "s/(CURRENT_PROJECT_VERSION: \")[0-9]+(\")/\1$NEXT\2/" project.yml
fi

echo "==> Generating project"
xcodegen generate >/dev/null

rm -rf build
AUTH=(-allowProvisioningUpdates -authenticationKeyPath "$P8" -authenticationKeyID "$KID" -authenticationKeyIssuerID "$ISS")

echo "==> Archiving"
xcodebuild archive -project HomeAccess.xcodeproj -scheme HomeAccess -configuration Release \
  -destination 'generic/platform=iOS' -archivePath build/HomeAccess.xcarchive "${AUTH[@]}" >/dev/null

echo "==> Exporting .ipa"
xcodebuild -exportArchive -archivePath build/HomeAccess.xcarchive -exportPath build/export \
  -exportOptionsPlist ExportOptions.plist "${AUTH[@]}" >/dev/null

echo "==> Uploading to TestFlight"
xcrun altool --upload-app -f build/export/HomeAccess.ipa -t ios --apiKey "$KID" --apiIssuer "$ISS"

echo "==> Done. Build is processing in App Store Connect (watch TestFlight)."
