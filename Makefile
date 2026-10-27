BINARY := loupe
GOFLAGS := -trimpath

LABEL := dev.bmcreations.loupe
PLIST := $(HOME)/Library/LaunchAgents/$(LABEL).plist
GUI   := gui/$(shell id -u)

.PHONY: build hotswap speccompare appmap composeagent simcap xcrunner test vet fmt run synthetic clean dist \
	agent-install agent-uninstall agent-restart agent-status agent-log

build:
	go build $(GOFLAGS) -o $(BINARY) ./cmd/loupe

# The hotswap extension (extensions/hotswap, docs/hotswap.md). A separate binary,
# not part of build: it reaches loupe only over HTTP.
hotswap:
	go build $(GOFLAGS) -o .build/hotswap ./extensions/hotswap

# The spec comparison extension (extensions/speccompare, docs/spec-compare.md).
speccompare:
	go build $(GOFLAGS) -o .build/speccompare ./extensions/speccompare

# The app map extension (extensions/appmap, docs/app-map.md).
appmap:
	go build $(GOFLAGS) -o .build/appmap ./extensions/appmap

# The Compose inspector agent (internal/composetree). Its output is committed
# and pinned, so build needs neither the NDK nor d8; rerun this after changing
# tools/composeagent and repin SHA256 in internal/composetree/agent.go.
NDK         ?= $(HOME)/Library/Android/sdk/ndk/29.0.14206865
ANDROID_JAR ?= $(HOME)/Library/Android/sdk/platforms/android-36/android.jar
D8          ?= $(HOME)/Library/Android/sdk/build-tools/36.0.0/d8
NDK_BIN     := $(NDK)/toolchains/llvm/prebuilt/darwin-x86_64/bin
COMPOSE_OUT := internal/composetree/agent

composeagent:
	$(NDK_BIN)/aarch64-linux-android29-clang -shared -fPIC -O2 -llog tools/composeagent/agent.c -o $(COMPOSE_OUT)/libloupecompose-arm64-v8a.so
	$(NDK_BIN)/x86_64-linux-android29-clang -shared -fPIC -O2 -llog tools/composeagent/agent.c -o $(COMPOSE_OUT)/libloupecompose-x86_64.so
	rm -rf .build/composeagent && mkdir -p .build/composeagent/classes
	javac -source 17 -target 17 -Xlint:-options -cp $(ANDROID_JAR) -d .build/composeagent/classes \
		$$(find tools/composeagent/src -name '*.java')
	cd .build/composeagent/classes && jar cf ../classes.jar .
	$(D8) --min-api 29 --lib $(ANDROID_JAR) --output .build/composeagent .build/composeagent/classes.jar
	cp .build/composeagent/classes.dex $(COMPOSE_OUT)/loupe-compose.dex

# simcap, the iOS simulator and iPhone helper, is closed source and ships as a
# signed, notarized binary on the releases page. The server lists simulators
# only once .build/simcap exists.
SIMCAP_URL ?= https://github.com/bmcreations/loupe/releases/latest/download/simcap-macos.zip
# The Developer ID team release builds are signed by. The download is refused
# unless its leaf certificate names this team.
SIMCAP_TEAM ?= 2X56L589EV

simcap:
	mkdir -p .build && rm -rf .build/simcap .build/simcap-dl
	curl -fsSL -o .build/simcap.zip $(SIMCAP_URL)
	ditto -x -k .build/simcap.zip .build/simcap-dl
	codesign --verify --strict -R='anchor apple generic and certificate leaf[subject.OU] = "$(SIMCAP_TEAM)"' .build/simcap-dl/simcap
	mv .build/simcap-dl/simcap .build/simcap
	rm -rf .build/simcap-dl .build/simcap.zip

# The XCUITest runner that applies a physical iPhone's touches below iOS 27,
# signed with TEAM, a development team ID from Xcode's Accounts. Built once
# for any iPhone; the server installs and starts it with test-without-building.
# BUNDLE_PREFIX replaces dev.bmcreations.loupe in its bundle IDs, which a team
# other than the one that registered them needs.
xcrunner:
	@test -n "$(TEAM)" || { echo "usage: make xcrunner TEAM=<development team id>"; exit 2; }
	cd tools/xcrunner && xcodegen generate --quiet
	xcodebuild build-for-testing -project tools/xcrunner/LoupeRunner.xcodeproj -scheme LoupeRunner \
		-destination generic/platform=iOS -derivedDataPath .build/xcrunner-dd \
		DEVELOPMENT_TEAM=$(TEAM) $(if $(BUNDLE_PREFIX),LOUPE_BUNDLE_PREFIX=$(BUNDLE_PREFIX)) -allowProvisioningUpdates -quiet
	rm -rf .build/xcrunner && cp -R .build/xcrunner-dd/Build/Products .build/xcrunner

test:
	go test ./...
	node web/run-tests.mjs

vet:
	go vet ./...

fmt:
	gofmt -w .

# Validate the client half with no Android hardware attached.
synthetic: build
	./$(BINARY) -synthetic -no-devices

run: build
	./$(BINARY)

# Cross-compile for the usual self-hosted targets. No cgo, so these are fully
# static and need nothing installed on the host they land on.
dist:
	@mkdir -p dist
	GOOS=darwin  GOARCH=arm64 go build $(GOFLAGS) -o dist/$(BINARY)-darwin-arm64  ./cmd/loupe
	GOOS=linux   GOARCH=amd64 go build $(GOFLAGS) -o dist/$(BINARY)-linux-amd64   ./cmd/loupe
	GOOS=linux   GOARCH=arm64 go build $(GOFLAGS) -o dist/$(BINARY)-linux-arm64   ./cmd/loupe
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -o dist/$(BINARY)-windows-amd64.exe ./cmd/loupe

clean:
	rm -rf $(BINARY) dist

# --- launchd ---------------------------------------------------------------
# The agent runs loupe-supervisor.sh, which runs a COPY of ./loupe and restarts
# onto a new build when ./loupe's sha256 changes. So `make build` is the deploy
# step: build, and the running server swaps itself within a couple of seconds.

agent-install: build
	@mkdir -p $(HOME)/Library/LaunchAgents
	@sed 's|__ROOT__|$(CURDIR)|g' launchd/$(LABEL).plist > $(PLIST)
	@launchctl bootout $(GUI)/$(LABEL) 2>/dev/null || true
	@launchctl bootstrap $(GUI) $(PLIST)
	@echo "loaded $(LABEL) -> $(PLIST)"

agent-uninstall:
	@launchctl bootout $(GUI)/$(LABEL) 2>/dev/null || true
	@rm -f $(PLIST)
	@echo "removed $(LABEL)"

agent-restart:
	@launchctl kickstart -k $(GUI)/$(LABEL)

agent-status:
	@launchctl print $(GUI)/$(LABEL) 2>/dev/null | grep -E 'state|pid|last exit|program' || echo "$(LABEL) is not loaded"

agent-log:
	@tail -n 40 -f loupe-run.log
