VERSION ?= $(shell date +%Y.%m.%d)

WORK  := build/pkg
DIST  := dist
CACHE := build/cache

# 7-Zip 24.09 static Linux x64 — only used during `make package` on the build host
7ZZ_URL     := https://www.7-zip.org/a/7z2501-linux-x64.tar.xz
7ZZ_ARCHIVE := $(CACHE)/7zz-linux.tar.xz
# SHA-256 of the pinned tarball above; verified across independent fetches.
# Bump together with 7ZZ_URL. This binary runs as root on every user's box.
7ZZ_SHA256  := 4ca3b7c6f2f67866b92622818b58233dc70367be2f36b498eb0bdeaaa44b53f4

# FFmpeg 7.0.2 fully-static Linux x64 (johnvansickle.com build, GPL v3).
# Includes decoders: h264/hevc/mpeg4/mpeg2/vc1/vp9/av1/dts/ac3/eac3/aac/mp3.
# Includes encoders: libx264, aac.  Muxers: mpegts, mp4.
# Does NOT include VAAPI (requires runtime libva — incompatible with static linking).
# SHA-256 verified across independent fetches; bump together with FFMPEG_URL.
# These binaries run as root on every user's box.
#
# ffmpeg ships as a SEPARATE companion plugin (filebrowser-ffmpeg), not inside
# the main package: the .plg re-downloads and rewrites the whole txz onto the
# USB boot flash on every update, and 190 MB of flash writes per file-browser
# release is not an acceptable price for a 5 MB app. The companion is versioned
# by ffmpeg's own version, so it is only rewritten when ffmpeg itself changes.
FFMPEG_VERSION := 7.0.2
FFMPEG_URL     := https://johnvansickle.com/ffmpeg/releases/ffmpeg-$(FFMPEG_VERSION)-amd64-static.tar.xz
FFMPEG_ARCHIVE := $(CACHE)/ffmpeg-linux64-static.tar.xz
FFMPEG_SHA256  := abda8d77ce8309141f83ab8edf0596834087c52467f6badf376a6a2a4c87cf67

# Companion plugin staging/artefact names.
FFMPEG_NAME := filebrowser-ffmpeg
FFMPEG_WORK := build/pkg-ffmpeg
FFMPEG_TXZ  := $(DIST)/$(FFMPEG_NAME)-$(FFMPEG_VERSION)-x86_64-1.txz
# Private bin directory: the companion owns it outright, so it can neither
# shadow nor be shadowed by an ffmpeg the admin installed another way.
FFMPEG_BINDIR := usr/local/filebrowser/bin

.PHONY: leakcheck all daemon daemon-linux web test dev \
        package package-ffmpeg package-all ffmpeg-cache clean

# ---------------------------------------------------------------------------
# make_txz — $(call make_txz,<txz path>,<staged tree>)
#
# Everything in a package is executed or served as root on the user's box and
# installpkg preserves the ownership recorded in the archive, so the build
# host's uid/gid must never leak into it: files owned by a non-root uid would
# be root-executed and non-root-writable at once. Build with forced root
# ownership, then re-read the finished archive and fail if anything is not 0/0.
# ---------------------------------------------------------------------------
define make_txz
	@if tar --version 2>/dev/null | grep -q GNU; then \
	  tar --owner=0 --group=0 --numeric-owner -cJf "$(1)" -C "$(2)" .; \
	elif tar --uid 0 --gid 0 --uname root --gname root -cf /dev/null -T /dev/null >/dev/null 2>&1; then \
	  echo "Note: GNU tar not found; using bsdtar with root ownership flags and COPYFILE_DISABLE."; \
	  COPYFILE_DISABLE=1 tar --uid 0 --gid 0 --uname root --gname root -cJf "$(1)" -C "$(2)" .; \
	else \
	  echo "ERROR: no tar that can force root ownership was found." >&2; \
	  echo "       GNU tar (--owner/--group) is absent and this bsdtar rejects --uid/--gid/--uname/--gname," >&2; \
	  echo "       so the package would ship files owned by uid $$(id -u):$$(id -g). installpkg preserves" >&2; \
	  echo "       that, leaving root-executed plugin files writable by a non-root account on the server." >&2; \
	  echo "       Install GNU tar (e.g. 'brew install gnu-tar' and put gtar first on PATH) and retry." >&2; \
	  exit 1; \
	fi
	@# GNU tar prints ownership as "uid/gid" in field 2; bsdtar prints uid and
	@# gid as fields 3 and 4 (field 2 is the link count). Handle both.
	@BAD=$$(tar --numeric-owner -tvJf "$(1)" \
	        | awk '{ if ($$2 ~ /\//) { if ($$2 != "0/0") print } else if ($$3 != "0" || $$4 != "0") print }'); \
	if [ -n "$$BAD" ]; then \
	  echo "ERROR: package contains entries not owned by root (0/0):" >&2; \
	  echo "$$BAD" >&2; \
	  exit 1; \
	fi; \
	echo "  Ownership: all entries 0/0 (root:root)"
endef

all: daemon web

# ---------------------------------------------------------------------------
# daemon — host architecture, for local dev/testing
# ---------------------------------------------------------------------------
daemon:
	cd daemon && CGO_ENABLED=0 go build -trimpath \
	  -ldflags "-s -w -X main.version=$(VERSION)" \
	  -o filebrowserd ./cmd/filebrowserd

# ---------------------------------------------------------------------------
# daemon-linux — static linux/amd64 for Unraid deployment
# ---------------------------------------------------------------------------
daemon-linux:
	cd daemon && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
	  -ldflags "-s -w -X main.version=$(VERSION)" \
	  -o filebrowserd-linux-amd64 ./cmd/filebrowserd

# ---------------------------------------------------------------------------
# web — install npm deps and build the React SPA to web/dist/
# ---------------------------------------------------------------------------
web:
	cd web && npm install --no-fund --no-audit && npm run build

# ---------------------------------------------------------------------------
# test — Go vet + tests; TypeScript typecheck if web/package.json is present
# ---------------------------------------------------------------------------
test: leakcheck
	cd daemon && go vet ./... && go test ./...
	@if [ -f web/package.json ]; then \
	  echo "--- web typecheck ---"; \
	  cd web && npm run typecheck; \
	fi

# ---------------------------------------------------------------------------
# dev — echo startup instructions; does not orchestrate processes
# ---------------------------------------------------------------------------
dev:
	@echo ""
	@echo "Start each in a separate terminal:"
	@echo ""
	@echo "  daemon:"
	@echo "    cd daemon && go run ./cmd/filebrowserd \\"
	@echo "        -dev -listen 127.0.0.1:8384 -root \$$HOME -data /tmp/fbdata"
	@echo ""
	@echo "  web (Vite dev server, proxies /api to 127.0.0.1:8384):"
	@echo "    cd web && npm run dev"
	@echo ""
	@echo "The SPA will be available at the URL Vite prints (typically http://localhost:5173)."
	@echo "In -dev mode auth is disabled and CORS is open."
	@echo ""

# ---------------------------------------------------------------------------
# package — build the Slackware-style txz and patch the .plg manifest
#
# Entity names expected in plugin/filebrowser.plg:
#   <!ENTITY version "...">
#   <!ENTITY md5 "...">
# Both are replaced in-place (sed) when copying the .plg to dist/.
# ---------------------------------------------------------------------------
package: daemon-linux web
	@# --- stage tree ---
	rm -rf "$(WORK)"
	mkdir -p \
	  "$(WORK)/usr/local/emhttp/plugins/filebrowser/app" \
	  "$(WORK)/usr/local/sbin"

	@# Plugin PHP/page files (FileBrowser.page, Settings.page, proxy.php, rc.filebrowserd, event/)
	cp -R plugin/source/filebrowser/. "$(WORK)/usr/local/emhttp/plugins/filebrowser/"

	@# Web SPA static assets
	cp -R web/dist/. "$(WORK)/usr/local/emhttp/plugins/filebrowser/app/"
	@# Read by FileBrowser.page to cache-bust the iframe URL on every update.
	printf '%s\n' "$(VERSION)" > "$(WORK)/usr/local/emhttp/plugins/filebrowser/VERSION"

	@# Daemon binary
	cp daemon/filebrowserd-linux-amd64 "$(WORK)/usr/local/sbin/filebrowserd"
	chmod 755 "$(WORK)/usr/local/sbin/filebrowserd"

	@# 7zz static binary — download once, cache in build/cache/
	@if [ ! -f "$(CACHE)/7zz" ]; then \
	  echo "Downloading 7zz static binary ..."; \
	  mkdir -p "$(CACHE)"; \
	  curl -fL "$(7ZZ_URL)" -o "$(7ZZ_ARCHIVE)"; \
	  if command -v sha256sum >/dev/null 2>&1; then \
	    GOT=$$(sha256sum "$(7ZZ_ARCHIVE)" | cut -d' ' -f1); \
	  else \
	    GOT=$$(shasum -a 256 "$(7ZZ_ARCHIVE)" | cut -d' ' -f1); \
	  fi; \
	  if [ "$$GOT" != "$(7ZZ_SHA256)" ]; then \
	    echo "ERROR: 7zz checksum mismatch: got $$GOT want $(7ZZ_SHA256)" >&2; \
	    rm -f "$(7ZZ_ARCHIVE)"; exit 1; \
	  fi; \
	  tar -xJf "$(7ZZ_ARCHIVE)" -C "$(CACHE)" 7zz; \
	  echo "7zz cached at $(CACHE)/7zz"; \
	fi
	cp "$(CACHE)/7zz" "$(WORK)/usr/local/sbin/7zz"
	chmod 755 "$(WORK)/usr/local/sbin/7zz"

	@# NOTE: ffmpeg/ffprobe are deliberately NOT staged here. They ship as the
	@# separate filebrowser-ffmpeg companion plugin — see `make package-ffmpeg`.

	@# --- build txz ---
	mkdir -p "$(DIST)"
	$(call make_txz,$(DIST)/filebrowser-$(VERSION)-x86_64-1.txz,$(WORK))

	@# --- compute MD5 and patch .plg ---
	@TXZ="$(DIST)/filebrowser-$(VERSION)-x86_64-1.txz"; \
	if command -v md5sum >/dev/null 2>&1; then \
	  MD5=$$(md5sum "$$TXZ" | awk '{print $$1}'); \
	else \
	  MD5=$$(md5 -q "$$TXZ"); \
	fi; \
	echo ""; \
	echo "  Package : $$TXZ"; \
	echo "  MD5     : $$MD5"; \
	echo ""; \
	sed \
	  -e "s|<!ENTITY version \"[^\"]*\"|<!ENTITY version \"$(VERSION)\"|g" \
	  -e "s|<!ENTITY md5 \"[^\"]*\"|<!ENTITY md5 \"$$MD5\"|g" \
	  plugin/filebrowser.plg > "$(DIST)/filebrowser.plg"; \
	echo "  PLG     : $(DIST)/filebrowser.plg (version + md5 entities patched)"

# ---------------------------------------------------------------------------
# ffmpeg-cache — download + SHA-256 verify the static ffmpeg/ffprobe once
# ---------------------------------------------------------------------------
ffmpeg-cache:
	@if [ ! -f "$(CACHE)/ffmpeg" ] || [ ! -f "$(CACHE)/ffprobe" ]; then \
	  echo "Downloading ffmpeg static binary ..."; \
	  mkdir -p "$(CACHE)"; \
	  curl -fL "$(FFMPEG_URL)" -o "$(FFMPEG_ARCHIVE)"; \
	  if command -v sha256sum >/dev/null 2>&1; then \
	    GOT=$$(sha256sum "$(FFMPEG_ARCHIVE)" | cut -d' ' -f1); \
	  else \
	    GOT=$$(shasum -a 256 "$(FFMPEG_ARCHIVE)" | cut -d' ' -f1); \
	  fi; \
	  if [ "$$GOT" != "$(FFMPEG_SHA256)" ]; then \
	    echo "ERROR: ffmpeg checksum mismatch: got $$GOT want $(FFMPEG_SHA256)" >&2; \
	    rm -f "$(FFMPEG_ARCHIVE)"; exit 1; \
	  fi; \
	  FFMPEG_TOPDIR=$$(tar -tJf "$(FFMPEG_ARCHIVE)" | head -1 | sed 's|/.*||'); \
	  tar -xJf "$(FFMPEG_ARCHIVE)" -C "$(CACHE)" \
	      "$${FFMPEG_TOPDIR}/ffmpeg" "$${FFMPEG_TOPDIR}/ffprobe"; \
	  mv "$(CACHE)/$${FFMPEG_TOPDIR}/ffmpeg"  "$(CACHE)/ffmpeg"; \
	  mv "$(CACHE)/$${FFMPEG_TOPDIR}/ffprobe" "$(CACHE)/ffprobe"; \
	  rm -rf "$(CACHE)/$${FFMPEG_TOPDIR}"; \
	  echo "ffmpeg/ffprobe cached at $(CACHE)/"; \
	fi

# ---------------------------------------------------------------------------
# package-ffmpeg — build the filebrowser-ffmpeg companion package + manifest
#
# Versioned by ffmpeg's own version, NOT the file browser's date version, so
# the 190 MB txz is only rewritten to the USB flash when ffmpeg itself is
# bumped. Same entity names as the main manifest:
#   <!ENTITY version "...">
#   <!ENTITY md5 "...">
# ---------------------------------------------------------------------------
package-ffmpeg: ffmpeg-cache
	@# --- stage tree ---
	rm -rf "$(FFMPEG_WORK)"
	mkdir -p "$(FFMPEG_WORK)/$(FFMPEG_BINDIR)"
	cp "$(CACHE)/ffmpeg"  "$(FFMPEG_WORK)/$(FFMPEG_BINDIR)/ffmpeg"
	cp "$(CACHE)/ffprobe" "$(FFMPEG_WORK)/$(FFMPEG_BINDIR)/ffprobe"
	chmod 755 "$(FFMPEG_WORK)/$(FFMPEG_BINDIR)/ffmpeg" \
	          "$(FFMPEG_WORK)/$(FFMPEG_BINDIR)/ffprobe"

	@# --- build txz ---
	mkdir -p "$(DIST)"
	$(call make_txz,$(FFMPEG_TXZ),$(FFMPEG_WORK))

	@# --- compute MD5 and patch .plg ---
	@TXZ="$(FFMPEG_TXZ)"; \
	if command -v md5sum >/dev/null 2>&1; then \
	  MD5=$$(md5sum "$$TXZ" | awk '{print $$1}'); \
	else \
	  MD5=$$(md5 -q "$$TXZ"); \
	fi; \
	echo ""; \
	echo "  Package : $$TXZ"; \
	echo "  MD5     : $$MD5"; \
	echo ""; \
	sed \
	  -e "s|<!ENTITY version \"[^\"]*\"|<!ENTITY version \"$(FFMPEG_VERSION)\"|g" \
	  -e "s|<!ENTITY md5 \"[^\"]*\"|<!ENTITY md5 \"$$MD5\"|g" \
	  plugin/$(FFMPEG_NAME).plg > "$(DIST)/$(FFMPEG_NAME).plg"; \
	echo "  PLG     : $(DIST)/$(FFMPEG_NAME).plg (version + md5 entities patched)"

# ---------------------------------------------------------------------------
# package-all — both plugins: the file browser and its ffmpeg companion
# ---------------------------------------------------------------------------
package-all: package package-ffmpeg

# ---------------------------------------------------------------------------
# clean
#   build/  = pkg (main staging), pkg-ffmpeg (companion staging), cache
#   dist/   = filebrowser*.txz + filebrowser*.plg for both plugins
# ---------------------------------------------------------------------------
clean:
	rm -rf build dist daemon/filebrowserd daemon/filebrowserd-* web/dist

# Refuse to ship private network details (hostnames, LAN IPs, MACs). Private
# patterns live in the git-ignored .leakcheck.local; see scripts/leakcheck.sh.
leakcheck:
	@scripts/leakcheck.sh
