BINARY := loupe
GOFLAGS := -trimpath

LABEL := dev.bmcreations.loupe
PLIST := $(HOME)/Library/LaunchAgents/$(LABEL).plist
GUI   := gui/$(shell id -u)

.PHONY: build composeagent a11ydump simcap camfeed xcrunner test vet fmt run synthetic clean dist \
	agent-install agent-uninstall agent-restart agent-status agent-log

# A checkout can add its own targets in private.mk, which is not part of this
# repo. One that sets SIMCAP_FROM_SOURCE replaces the simcap target below with
# its own.
.DEFAULT_GOAL := build
-include private.mk

build:
	go build $(GOFLAGS) -o $(BINARY) ./cmd/loupe

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

# The Android tree dump (internal/a11ydump). Committed and pinned like the
# compose agent; rerun after changing tools/a11ydump and repin SHA256 in
# internal/a11ydump/a11ydump.go.
a11ydump:
	rm -rf .build/a11ydump && mkdir -p .build/a11ydump/classes
	javac -source 17 -target 17 -Xlint:-options -cp $(ANDROID_JAR) -d .build/a11ydump/classes \
		$$(find tools/a11ydump/src -name '*.java')
	cd .build/a11ydump/classes && jar cf ../classes.jar .
	$(D8) --min-api 29 --lib $(ANDROID_JAR) --output .build/a11ydump .build/a11ydump/classes.jar
	cp .build/a11ydump/classes.dex internal/a11ydump/loupe-a11y.dex

# simcap, the iOS simulator and iPhone helper, lives in bmcreations/simcap.
# This clones it into .build/simcap-src (or updates the clone) and builds it
# with Xcode's swiftc. The server lists simulators only once .build/simcap
# exists. TEAM signs it with that team's Apple Development identity, so a
# phone's camera grant survives a rebuild; without it the build is ad hoc.
#
# simcap-bridge, the Rust helper that reads a cabled iPhone's accessibility
# tree over its own CoreDevice tunnel, is built and copied beside simcap when
# the cloned revision carries a `bridge` target. The `make -n bridge` probe
# keeps this a no-op against revisions that predate it, so `ui ios:` is
# unavailable rather than the build broken.
#
# .build/simcap.ref records the commit each build came from, and
# tools/simcap-guard.sh refuses to replace a simcap whose changes SIMCAP_REF
# does not carry, or one with no record: that build may be an unmerged branch a
# running server depends on. FORCE=1 replaces it anyway.
SIMCAP_REPO ?= https://github.com/bmcreations/simcap.git
SIMCAP_REF ?= main

ifndef SIMCAP_FROM_SOURCE
simcap:
	mkdir -p .build
	if [ -d .build/simcap-src/.git ]; then \
		git -C .build/simcap-src fetch -q --depth 1 origin $(SIMCAP_REF) && \
		git -C .build/simcap-src checkout -q --detach FETCH_HEAD; \
	else \
		rm -rf .build/simcap-src && \
		git clone -q --depth 1 --branch $(SIMCAP_REF) $(SIMCAP_REPO) .build/simcap-src; \
	fi
	FORCE=$(FORCE) tools/simcap-guard.sh .build/simcap-src $(SIMCAP_REF)
	$(MAKE) -C .build/simcap-src simcap TEAM=$(TEAM)
	cp .build/simcap-src/.build/simcap .build/simcap.new
	mv -f .build/simcap.new .build/simcap
	git -C .build/simcap-src rev-parse HEAD > .build/simcap.ref
	if $(MAKE) -C .build/simcap-src -n bridge >/dev/null 2>&1; then \
		$(MAKE) -C .build/simcap-src bridge TEAM=$(TEAM) && \
		cp .build/simcap-src/.build/simcap-bridge .build/simcap-bridge.new && \
		mv -f .build/simcap-bridge.new .build/simcap-bridge; \
	fi
endif

# loupe-camfeed writes the frames clients send for an AVD on Handset Camera
# into the camera Handset installs (internal/camfeed). Handset bundles a
# prebuilt copy beside its loupe; a checkout builds its own into .build.
camfeed:
	mkdir -p .build
	xcrun -sdk macosx swiftc -O -o .build/loupe-camfeed.new tools/camfeed/main.swift
	mv -f .build/loupe-camfeed.new .build/loupe-camfeed

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

#
# bootout returns before launchd has dropped the old service, and a bootstrap
# in that window fails with "5: Input/output error", leaving no agent loaded.
# So wait for the label to go, then retry the bootstrap.
#
# The agent runs from this checkout and finds simcap, simcap-bridge and the
# XCUITest runner in its .build, which a fresh checkout or worktree lacks.
agent-install: build
	@mkdir -p $(HOME)/Library/LaunchAgents
	@sed 's|__ROOT__|$(CURDIR)|g' launchd/$(LABEL).plist > $(PLIST)
	@launchctl bootout $(GUI)/$(LABEL) 2>/dev/null || true
	@for _ in $$(seq 1 20); do launchctl print $(GUI)/$(LABEL) >/dev/null 2>&1 || break; sleep 0.25; done
	@for i in 1 2 3; do \
		launchctl bootstrap $(GUI) $(PLIST) && break; \
		[ $$i = 3 ] && exit 1; sleep 1; \
	done
	@echo "loaded $(LABEL) -> $(PLIST)"
	@[ -e .build/simcap ] || echo "warning: no .build/simcap: no simulators or iPhones (make simcap)"
	@[ -e .build/simcap-bridge ] || echo "warning: no .build/simcap-bridge: no ui on a cabled iPhone (make simcap)"
	@[ -e .build/loupe-camfeed ] || echo "warning: no .build/loupe-camfeed: no live Handset Camera on emulators (make camfeed)"
	@[ -e .build/xcrunner ] || echo "warning: no .build/xcrunner: no touches on an iPhone below iOS 27 (make xcrunner TEAM=...)"

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
