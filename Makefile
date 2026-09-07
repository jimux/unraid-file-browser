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

.PHONY: leakcheck leakcheck all daemon daemon-linux web test dev package clean

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

	@# --- build txz ---
	mkdir -p "$(DIST)"
	@# Everything in the package is executed or served as root on the user's
	@# box and installpkg preserves the ownership recorded in the archive, so
	@# the build host's uid/gid must never leak into it: files owned by a
	@# non-root uid would be root-executed and non-root-writable at once.
	@if tar --version 2>/dev/null | grep -q GNU; then \
	  tar --owner=0 --group=0 --numeric-owner \
	      -cJf "$(DIST)/filebrowser-$(VERSION)-x86_64-1.txz" \
	      -C "$(WORK)" .; \
	elif tar --uid 0 --gid 0 --uname root --gname root -cf /dev/null -T /dev/null >/dev/null 2>&1; then \
	  echo "Note: GNU tar not found; using bsdtar with root ownership flags and COPYFILE_DISABLE."; \
	  COPYFILE_DISABLE=1 tar --uid 0 --gid 0 --uname root --gname root \
	      -cJf "$(DIST)/filebrowser-$(VERSION)-x86_64-1.txz" \
	      -C "$(WORK)" .; \
	else \
	  echo "ERROR: no tar that can force root ownership was found." >&2; \
	  echo "       GNU tar (--owner/--group) is absent and this bsdtar rejects --uid/--gid/--uname/--gname," >&2; \
	  echo "       so the package would ship files owned by uid $$(id -u):$$(id -g). installpkg preserves" >&2; \
	  echo "       that, leaving root-executed plugin files writable by a non-root account on the server." >&2; \
	  echo "       Install GNU tar (e.g. 'brew install gnu-tar' and put gtar first on PATH) and retry." >&2; \
	  exit 1; \
	fi
	@# Fail the build rather than ship a package with a non-root owner in it.
	@# GNU tar prints ownership as "uid/gid" in field 2; bsdtar prints uid and
	@# gid as fields 3 and 4 (field 2 is the link count). Handle both.
	@BAD=$$(tar --numeric-owner -tvJf "$(DIST)/filebrowser-$(VERSION)-x86_64-1.txz" \
	        | awk '{ if ($$2 ~ /\//) { if ($$2 != "0/0") print } else if ($$3 != "0" || $$4 != "0") print }'); \
	if [ -n "$$BAD" ]; then \
	  echo "ERROR: package contains entries not owned by root (0/0):" >&2; \
	  echo "$$BAD" >&2; \
	  exit 1; \
	fi; \
	echo "  Ownership: all entries 0/0 (root:root)"

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
# clean
# ---------------------------------------------------------------------------
clean:
	rm -rf build dist daemon/filebrowserd daemon/filebrowserd-* web/dist

# Refuse to ship private network details (hostnames, LAN IPs, MACs). Private
# patterns live in the git-ignored .leakcheck.local; see scripts/leakcheck.sh.
leakcheck:
	@scripts/leakcheck.sh
