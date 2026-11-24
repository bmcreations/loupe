// Package firebase reads what Google's android CLI prints for Firebase Device
// Streaming reservations (`android device remote ...`) and checks the
// project and device names loupe passes it.
//
// Running the CLI, the reservation state file and the billing rules stay with
// httpapi.RemoteDevices. A connected reservation is an ordinary adb device at
// localhost:<port>, so nothing here streams or takes input.
package firebase

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	validProject  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,29}$`)
	validCodename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+-]{0,63}$`)
)

// ValidProject reports whether s is a Google Cloud project id.
func ValidProject(s string) bool { return validProject.MatchString(s) }

// ValidCodename reports whether s can be a device codename, as `device
// remote models` lists them.
func ValidCodename(s string) bool { return validCodename.MatchString(s) }

// Model is one reservable device and the API levels it comes with.
type Model struct {
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model"`
	Codename     string `json:"codename"`
	APIs         []int  `json:"apis"`
}

// Created is what `device remote create --connect` printed: the reservation
// id, the adb port it connected on and the wall-clock time it ends. Each is
// zero when the output does not carry it.
type Created struct {
	ID      string
	Port    int
	Expires string
}

// ListRow is one row of `device remote list`.
type ListRow struct {
	ID, State, Expires, Manufacturer, Model, Codename string
	API                                               int
}

var (
	reCreatedID = regexp.MustCompile(`New reservation created with id (\S+?)\.`)
	reEndsAt    = regexp.MustCompile(`Reservation ends at (\d{1,2}:\d{2}[\s\x{a0}\x{202f}]*[AP]M)`)
	rePort      = regexp.MustCompile(`Device connected on port (\d+)`)
	reClock     = regexp.MustCompile(`^(\d{1,2}):(\d{2})[\s\x{a0}\x{202f}]*([AP]M)$`)
	// Two or more plain spaces: the CLI writes "10:56 AM" with a narrow
	// no-break space, which must stay inside its column.
	reColumns = regexp.MustCompile(` {2,}`)
	// reListHeader is the first line of `device remote list` when the
	// project holds anything.
	reListHeader = regexp.MustCompile(`(?m)^\s*Reservation {2,}State {2,}`)
	reAccount    = regexp.MustCompile(`(?m)^Logged in as (.+)$`)
)

// ParseCreate reads the output of `device remote create --connect`.
func ParseCreate(out string) Created {
	var c Created
	if m := reCreatedID.FindStringSubmatch(out); m != nil {
		c.ID = m[1]
	}
	if m := reEndsAt.FindStringSubmatch(out); m != nil {
		c.Expires = m[1]
	}
	if m := rePort.FindStringSubmatch(out); m != nil {
		c.Port, _ = strconv.Atoi(m[1])
	}
	return c
}

// ResolveClock turns the CLI's "10:56 AM" into a time: that clock time today,
// or tomorrow when today's is more than a minute past. Zero when unreadable.
func ResolveClock(clock string, now time.Time) time.Time {
	m := reClock.FindStringSubmatch(strings.TrimSpace(clock))
	if m == nil {
		return time.Time{}
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	h %= 12
	if m[3] == "PM" {
		h += 12
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
	if t.Before(now.Add(-time.Minute)) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// IsList reports whether out is a reservation list `device remote list`
// recognisably printed, empty or not, so an empty answer is never taken for
// an empty project.
func IsList(out string) bool {
	return strings.Contains(out, "No reservations found") || reListHeader.MatchString(out)
}

// ParseList reads the rows of `device remote list`.
func ParseList(out string) []ListRow {
	var rows []ListRow
	for _, line := range strings.Split(out, "\n") {
		f := reColumns.Split(strings.TrimSpace(line), -1)
		if len(f) < 7 || f[0] == "Reservation" {
			continue
		}
		api, _ := strconv.Atoi(f[6])
		rows = append(rows, ListRow{f[0], f[1], f[2], f[3], f[4], f[5], api})
	}
	return rows
}

// ListsID reports whether any line of a list starts with id. It reads the raw
// line, not ParseList's row, so a row with a column missing still counts as
// held.
func ListsID(out, id string) bool {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == id {
			return true
		}
	}
	return false
}

// ParseModels reads the tree `device remote models` prints.
func ParseModels(out string) []Model {
	var models []Model
	maker := ""
	for _, line := range strings.Split(out, "\n") {
		body := strings.TrimLeft(line, "│├└─ \t")
		if body == "" {
			continue
		}
		indented := strings.HasPrefix(line, "│") || strings.HasPrefix(line, " ")
		f := reColumns.Split(strings.TrimSpace(body), -1)
		if !indented {
			// "Google [37]"
			if i := strings.LastIndex(body, " ["); i > 0 {
				maker = body[:i]
			}
			continue
		}
		if len(f) < 2 || maker == "" {
			continue
		}
		m := Model{Manufacturer: maker, Model: f[0]}
		for _, pair := range f[1:] {
			i := strings.LastIndex(pair, "/")
			if i <= 0 {
				continue
			}
			api, err := strconv.Atoi(pair[i+1:])
			if err != nil {
				continue
			}
			m.Codename = pair[:i]
			m.APIs = append(m.APIs, api)
		}
		if m.Codename != "" {
			sort.Sort(sort.Reverse(sort.IntSlice(m.APIs)))
			models = append(models, m)
		}
	}
	return models
}

// ParseProjects reads the project ids `device remote projects` lists, never
// nil.
func ParseProjects(out string) []string {
	projects := []string{}
	listing := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Projects available") {
			listing = true
			continue
		}
		if !listing {
			continue
		}
		if f := strings.Fields(line); len(f) > 0 && validProject.MatchString(f[0]) {
			projects = append(projects, f[0])
		}
	}
	return projects
}

// ParseAccount is the account `auth status` says the CLI is signed in as, or
// "" when it is signed out.
func ParseAccount(out string) string {
	if m := reAccount.FindStringSubmatch(out); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// APIDisabled reports whether a failure is the project not having the Device
// Streaming or Cloud Testing API turned on.
func APIDisabled(out string) bool {
	return strings.Contains(out, "SERVICE_DISABLED") || strings.Contains(out, "has not been used in project") || strings.Contains(out, "it is disabled")
}

// Failure picks the line worth showing from a failed CLI call.
func Failure(out, stderr string, runErr error) string {
	for _, text := range []string{stderr, out} {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		for _, l := range lines {
			if l = strings.TrimSpace(l); strings.HasPrefix(l, "Error:") {
				return strings.TrimSpace(strings.TrimPrefix(l, "Error:"))
			}
		}
		if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
			return last
		}
	}
	if runErr != nil {
		return runErr.Error()
	}
	return "the CLI finished without connecting a device"
}
