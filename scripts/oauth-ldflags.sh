# Emits -ldflags -X arguments that compile OAuth client credentials into the
# binary, read from oauth.env at the repository root.
#
# Sourced by build.sh and android/build-apk.sh. With no oauth.env present this
# emits nothing and the app asks for credentials on first use instead.
#
# oauth.env format (see oauth.env.example):
#   GOOGLE_CLIENT_ID=1234-abc.apps.googleusercontent.com
#   GOOGLE_CLIENT_SECRET=GOCSPX-...

oauth_ldflags() {
  local file="${1:-oauth.env}"
  [ -f "$file" ] || return 0

  local pkg="omnidrive/internal/provider"
  local out="" key value

  while IFS= read -r line || [ -n "$line" ]; do
    # Skip blanks and comments.
    case "$line" in ''|\#*) continue ;; esac

    key="${line%%=*}"
    value="${line#*=}"
    # Trim surrounding whitespace and optional quotes.
    key="$(printf '%s' "$key" | tr -d '[:space:]')"
    value="$(printf '%s' "$value" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//; s/^"\(.*\)"$/\1/; s/^'"'"'\(.*\)'"'"'$/\1/')"
    [ -n "$value" ] || continue

    case "$key" in
      GOOGLE_CLIENT_ID)       out="$out -X $pkg.BuiltinGoogleClientID=$value" ;;
      GOOGLE_CLIENT_SECRET)   out="$out -X $pkg.BuiltinGoogleSecret=$value" ;;
      ONEDRIVE_CLIENT_ID)     out="$out -X $pkg.BuiltinOneDriveClientID=$value" ;;
      ONEDRIVE_CLIENT_SECRET) out="$out -X $pkg.BuiltinOneDriveSecret=$value" ;;
      DROPBOX_CLIENT_ID)      out="$out -X $pkg.BuiltinDropboxClientID=$value" ;;
      DROPBOX_CLIENT_SECRET)  out="$out -X $pkg.BuiltinDropboxSecret=$value" ;;
      *) printf 'oauth.env: ignoring unknown key %s\n' "$key" >&2 ;;
    esac
  done < "$file"

  printf '%s' "$out"
}

# oauth_summary prints which providers this build will ship preconfigured.
oauth_summary() {
  local flags="$1"
  local names=""
  case "$flags" in *BuiltinGoogleClientID*)   names="$names Google" ;; esac
  case "$flags" in *BuiltinOneDriveClientID*) names="$names OneDrive" ;; esac
  case "$flags" in *BuiltinDropboxClientID*)  names="$names Dropbox" ;; esac
  if [ -n "$names" ]; then
    printf 'preconfigured:%s\n' "$names"
  fi
}
