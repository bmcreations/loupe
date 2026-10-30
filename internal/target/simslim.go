package target

// Slim simulator boots: launchd overrides that keep 168 background daemons
// from starting. On loupe-probe (iPhone 17 Pro, iOS 27.0) a slim boot ran 80
// processes against 215 and summed to a 2804 MB footprint against 8888 MB.
// Idle CPU 90s after boot was 2% of a core against 26-160%, though both settle
// near 1% within five minutes. Stream frame rate and lateness did not change.
//
// The label list is copied from simslim (github.com/mobai-app/simslim,
// profiles.go at 2c68da9), which carries this notice:
//
//	MIT License
//
//	Copyright (c) 2026 Interlap
//
//	Permission is hereby granted, free of charge, to any person obtaining a copy
//	of this software and associated documentation files (the "Software"), to deal
//	in the Software without restriction, including without limitation the rights
//	to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
//	copies of the Software, and to permit persons to whom the Software is
//	furnished to do so, subject to the following conditions:
//
//	The above copyright notice and this permission notice shall be included in all
//	copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
//	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
//	FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
//	AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
//	LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
//	OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
//	SOFTWARE.

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// simSlimLabels is every daemon a slim boot disables, grouped as simslim
// groups them. Labels simslim keeps on whatever the profile are not here.
var simSlimLabels = []string{
	// Widgets & Wallpaper
	"com.apple.PosterBoard",
	"com.apple.chronod",
	"com.apple.liveactivitiesd",
	"com.apple.navd",
	// Siri & Intelligence
	"com.apple.assistant_cdmd",
	"com.apple.assistant_service",
	"com.apple.siriactionsd",
	"com.apple.siriinferenced",
	"com.apple.siriknowledged",
	"com.apple.sirittsd",
	"com.apple.siri.context.service",
	"com.apple.siri.acousticsignature",
	"com.apple.corespeechd",
	"com.apple.voiced",
	"com.apple.voicebankingd",
	"com.apple.speechmodeltrainingd",
	"com.apple.intelligenceplatformd",
	"com.apple.intelligencecontextd",
	"com.apple.intelligenceflowd",
	"com.apple.intelligencetasksd",
	"com.apple.generativeexperiencesd",
	"com.apple.knowledgeconstructiond",
	"com.apple.naturallanguaged",
	"com.apple.textunderstandingd",
	"com.apple.modelcatalogd",
	"com.apple.modelmanagerd",
	"com.apple.mlhostd",
	"com.apple.mlruntimed",
	"com.apple.suggestd",
	"com.apple.parsecd",
	"com.apple.parsec-fbf",
	"com.apple.proactiveeventtrackerd",
	// Spotlight & Search
	"com.apple.searchd",
	"com.apple.searchtoold",
	"com.apple.spotlightknowledged",
	"com.apple.spotlightknowledged.updater",
	"com.apple.corespotlightservice",
	// iCloud & Apple Account
	"com.apple.appleaccountd",
	"com.apple.appleaccounttransparencyd",
	"com.apple.appleidsetupd",
	"com.apple.akd",
	"com.apple.amsaccountsd",
	"com.apple.amsengagementd",
	"com.apple.amsondevicestoraged",
	"com.apple.cloudd",
	"com.apple.cloudphotod",
	"com.apple.ckdiscretionaryd",
	"com.apple.cloudsettingssyncagent",
	"com.apple.bird",
	"com.apple.syncdefaultsd",
	"com.apple.cdpd",
	"com.apple.sosd",
	"com.apple.SecureBackupDaemon",
	"com.apple.TrustedPeersHelper",
	"com.apple.protectedcloudstorage.protectedcloudkeysyncing",
	"com.apple.icloudmailagent",
	"com.apple.icloudsubscriptionoptimizerd",
	"com.apple.communicationtrustd",
	// App Store, Push & Media
	"com.apple.appstored",
	"com.apple.appstorecomponentsd",
	"com.apple.apsd",
	"com.apple.itunescloudd",
	"com.apple.itunesstored",
	"com.apple.storekitd",
	"com.apple.passd",
	"com.apple.financed",
	"com.apple.videosubscriptionsd",
	"com.apple.assetsubscriptiond",
	"com.apple.musicd",
	// Mail, Calendar & Contacts
	"com.apple.email.maild",
	"com.apple.exchangesyncd",
	"com.apple.dataaccess.dataaccessd",
	"com.apple.calaccessd",
	"com.apple.remindd",
	"com.apple.contactsd",
	"com.apple.contacts.postersyncd",
	"com.apple.peopled",
	// Safari Sync & Web Services
	"com.apple.SafariBookmarksSyncAgent",
	"com.apple.Safari.History",
	"com.apple.Safari.passwordbreachd",
	"com.apple.Safari.SafeBrowsing.Service",
	"com.apple.safarifetcherd",
	"com.apple.WebBookmarks.webbookmarksd",
	"com.apple.webkit.adattributiond",
	"com.apple.webkit.webpushd",
	"com.apple.webprivacyd",
	"com.apple.swcd",
	// Family & Screen Time
	"com.apple.familycircled",
	"com.apple.FamilyControlsAgent",
	"com.apple.familynotification",
	"com.apple.askpermissiond",
	"com.apple.asktod",
	"com.apple.ScreenTimeAgent",
	"com.apple.ScreenTimeSettingsAgent",
	"com.apple.UsageTrackingAgent",
	// Health, Home & Fitness
	"com.apple.healthd",
	"com.apple.healthappd",
	"com.apple.healthcontentd",
	"com.apple.healtheventsd",
	"com.apple.healthrecordsd",
	"com.apple.finhealthd",
	"com.apple.homed",
	"com.apple.homeeventsd",
	"com.apple.fitcore",
	"com.apple.fitcore.session",
	"com.apple.fitnesscoachingd",
	"com.apple.fitnessintelligenced",
	"com.apple.activityawardsd",
	"com.apple.activitysharingd",
	// Photos & Media Analysis
	"com.apple.photoanalysisd",
	"com.apple.photosface",
	"com.apple.mediaanalysisd",
	"com.apple.mediaanalysisd.service",
	"com.apple.mediastream.mstreamd",
	"com.apple.medialibraryd",
	// com.apple.assetsd is not here though simslim disables it: it serves
	// com.apple.photos.service, and without it `simctl addmedia` fails with
	// PHPhotosErrorDomain 3301, so /api/media could add nothing.
	"com.apple.assetsd.nebulad",
	// News, Weather, Maps & Games
	"com.apple.newsd",
	"com.apple.weatherd",
	"com.apple.Maps.mapssyncd",
	"com.apple.Maps.mapspushd",
	"com.apple.Maps.geocorrectiond",
	"com.apple.maps.destinationd",
	"com.apple.MapKit.SnapshotService",
	"com.apple.jetpackassetd",
	"com.apple.tipsd",
	"com.apple.gamesaved",
	"com.apple.GameController.gamecontrollerd",
	// Messaging & FaceTime
	"com.apple.identityservicesd",
	"com.apple.ids_simd",
	"com.apple.imautomatichistorydeletionagent",
	"com.apple.imcore.imtransferagent",
	"com.apple.imdpersistence.IMDPersistenceAgent",
	"com.apple.facetimemessagestored",
	"com.apple.telephonyutilities.callservicesd",
	// Sharing & Device Connectivity
	"com.apple.rapportd",
	"com.apple.companiond",
	"com.apple.carkitd",
	"com.apple.wcd",
	"com.apple.tvremoted",
	"com.apple.avatarsd",
	"com.apple.stickersd",
	"com.apple.sociallayerd",
	"com.apple.announced",
	"com.apple.findmy.findmylocated",
	// Ads, Diagnostics & Telemetry
	"com.apple.ap.adprivacyd",
	"com.apple.ap.promotedcontentd",
	"com.apple.diagnosticextensionsd",
	"com.apple.feedbackd",
	"com.apple.rtcreportingd",
	"com.apple.securityuploadd",
	"com.apple.geoanalyticsd",
	"com.apple.triald",
	"com.apple.followupd",
	"com.apple.purplebuddy.budd",
	"com.apple.devicecheckd",
	// Other Background Services
	"com.apple.merchantd",
	"com.apple.coreidvd",
	"com.apple.businessservicesd",
	"com.apple.deviceaccessd",
	"com.apple.replicatord",
	"com.apple.linkd",
	"com.apple.ind",
	"com.apple.storagedatad",
	"com.apple.StatusKitAgent",
	"com.apple.countryd",
	"com.apple.mobileassetd",
	"com.apple.managedconfiguration.passcodenagd",
}

// simSlimRoot holds each simulator's launchd disable store. launchd_sim is a
// host process, so the store is not in the device's data directory: it is
// /private/var/tmp/com.apple.CoreSimulator.SimDevice.<udid>/disabled.plist,
// read when the simulator boots. It survives simctl erase, not simctl clone.
// A var so tests can point it elsewhere.
var simSlimRoot = "/private/var/tmp"

// simSlimMarker, beside the store, lists the labels loupe itself disabled.
// Turning slim off enables only those, so overrides a person set another way,
// simslim's included, are left as they were.
const simSlimMarker = "loupe-slim.txt"

func simSlimDir(udid string) string {
	return filepath.Join(simSlimRoot, "com.apple.CoreSimulator.SimDevice."+udid)
}

// slimPersists reports whether a runtime reads the store at boot. iOS 17 and
// 18.3 keep launchd overrides in memory only and boot stock whatever the file
// says; 18.5 and newer read it.
func slimPersists(osVersion string) bool {
	parts := strings.SplitN(osVersion, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil && (major > 18 || major == 18 && minor >= 5)
}

// setSlim rewrites a shut-down simulator's store so its next boot is slim, or
// undoes what an earlier setSlim did. It returns the labels a slim boot should
// come up with disabled, which is none when slim is off. Off with no marker
// writes nothing: loupe never touches a store it has not slimmed.
func setSlim(udid string, on bool) ([]string, error) {
	dir := simSlimDir(udid)
	marker := filepath.Join(dir, simSlimMarker)
	ours, err := readSlimMarker(marker)
	if err != nil {
		return nil, err
	}
	if !on && len(ours) == 0 {
		return nil, nil
	}
	store := filepath.Join(dir, "disabled.plist")
	entries, err := readSlimStore(store)
	if err != nil {
		return nil, err
	}
	if on {
		listed := make(map[string]bool, len(simSlimLabels))
		for _, l := range simSlimLabels {
			listed[l] = true
			if !entries[l] {
				entries[l] = true
				ours[l] = true
			}
		}
		// A label an older list disabled and this one dropped comes back on,
		// so a simulator slimmed before the drop heals on its next boot.
		for l := range ours {
			if !listed[l] {
				entries[l] = false
				delete(ours, l)
			}
		}
	} else {
		for l := range ours {
			entries[l] = false
		}
		ours = map[string]bool{}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := writeAtomic(store, encodeSlimStore(entries)); err != nil {
		return nil, err
	}
	if len(ours) == 0 {
		if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if on {
			return simSlimLabels, nil
		}
		return nil, nil
	}
	if err := writeAtomic(marker, []byte(strings.Join(sortedTrue(ours), "\n")+"\n")); err != nil {
		return nil, err
	}
	return simSlimLabels, nil
}

func readSlimMarker(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	ours := map[string]bool{}
	for _, l := range strings.Fields(string(b)) {
		ours[l] = true
	}
	return ours, nil
}

// readSlimStore returns every entry in the store, including the explicit
// false ones the runtime writes for itself, which a rewrite must keep. A
// simulator that has never booted has no store, which is nothing disabled.
func readSlimStore(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseSlimStore(f)
}

// parseSlimStore reads the flat <key>label</key><true/>|<false/> dict that
// launchd_sim writes, ignoring anything else in the file.
func parseSlimStore(r io.Reader) (map[string]bool, error) {
	entries := map[string]bool{}
	dec := xml.NewDecoder(r)
	label := ""
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse launchd disable store: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			if err := dec.DecodeElement(&label, &start); err != nil {
				return nil, fmt.Errorf("parse launchd disable store: %w", err)
			}
		case "true", "false":
			if label != "" {
				entries[label] = start.Name.Local == "true"
				label = ""
			}
		}
	}
}

// encodeSlimStore writes the store in launchd_sim's own shape, sorted so a
// rewrite that changes nothing writes the same bytes.
func encodeSlimStore(entries map[string]bool) []byte {
	labels := make([]string, 0, len(entries))
	for l := range entries {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	for _, l := range labels {
		b.WriteString("\t<key>")
		xml.EscapeText(&b, []byte(l))
		b.WriteString("</key>\n\t<")
		b.WriteString(strconv.FormatBool(entries[l]))
		b.WriteString("/>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func sortedTrue(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k, v := range set {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// writeAtomic replaces path in one rename, so launchd_sim never reads half a
// store.
func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// slimHonoured counts how many of wanted a booted simulator reports
// disabled. The store is an undocumented CoreSimulator detail, so this read
// back is the only evidence a slim boot took.
func slimHonoured(ctx context.Context, udid string, wanted []string) (int, error) {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl", "spawn", udid, "launchctl", "print-disabled", "system").Output()
	if err != nil {
		return 0, fmt.Errorf("launchctl print-disabled: %w", err)
	}
	off := parseDisabled(string(out))
	n := 0
	for _, l := range wanted {
		if off[l] {
			n++
		}
	}
	return n, nil
}

// parseDisabled reads launchctl print-disabled lines, `"label" => disabled`.
func parseDisabled(out string) map[string]bool {
	off := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		label, state, ok := strings.Cut(strings.TrimSpace(line), " => ")
		if ok && state == "disabled" {
			off[strings.Trim(label, `"`)] = true
		}
	}
	return off
}
