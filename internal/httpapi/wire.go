package httpapi

// ErrorResponse is the body every JSON error response on this API shares:
// one string naming what went wrong. It replaces what used to be an ad hoc
// map[string]string{"error": ...} built fresh at each call site -- the two
// marshal identically, but only a named type gives apischema (see
// internal/apischema) something to point a $ref at, and a caller something
// to unmarshal into without guessing the shape.
//
// The handful of plain-text 405s built with http.Error directly ("POST
// only", "GET only" in agent.go and events.go) are not this type: they
// predate it, are not JSON, and are left alone deliberately -- see
// internal/apischema's endpoints table for where they are named instead of
// schematized.
type ErrorResponse struct {
	Error string `json:"error"`
	// Code names an error a client acts on rather than only shows. It is
	// empty for every error but the ErrCode constants below.
	Code string `json:"code,omitempty"`
	// RetryAfterMs is how long to wait before trying again, set only with
	// ErrCodeHumanHeld.
	RetryAfterMs int64 `json:"retryAfterMs,omitempty"`
	// ID names the recording that caused an ErrCodeRecording 409 on a start,
	// when it is known.
	ID string `json:"id,omitempty"`
	// Claim is the claim that caused an ErrCodeClaimed 409.
	Claim *ClaimInfo `json:"claim,omitempty"`
	// Fingerprint is the certificate an ErrCodeConfirmFingerprint 409 asks
	// to have confirmed, as hex SHA-256 of its DER.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// ErrCodeHubHeld is POST /api/input's 409 when Device Hub holds an iOS
// simulator's buttons and keyboard and this server has not claimed it. The
// action that takes them back, "reclaim", closes that simulator's apps, so a
// client asks its user before sending it.
const ErrCodeHubHeld = "hubHeld"

// ErrCodeTouchRejected is POST /api/input's 409 when a tap or swipe reached
// the device and Android's input dispatcher threw it away, usually because a
// pointer is stuck down (docs/traps.md, trap 50). The error carries the
// dispatcher's reason. Nothing moved on the screen.
const ErrCodeTouchRejected = "touchRejected"

// ErrCodePhoneUnreachable is POST /api/input's 503 when a physical iPhone did
// not accept the input: CoreDevice refused it before anything was sent, the
// server woke the tunnel with devicectl and sent it once more, and CoreDevice
// refused again. Nothing reached the phone, so retrying is safe. `error` has
// CoreDevice's text and devicectl's outcome. It never covers a send that went
// out; that is ErrCodePhoneDeliveryUnknown.
const ErrCodePhoneUnreachable = "phoneUnreachable"

// ErrCodePhoneDeliveryUnknown is POST /api/input's 503 when a send to a
// physical iPhone went out and whether the phone received it is unknown:
// simcap reported a failure after sending, or did not reply in its budget.
// The input may have landed, so do not retry blindly: a second try could tap
// twice, press Home twice or double a character. CoreDevice gives no delivery
// receipt, so even a 200 means sent and the capability still answered.
const ErrCodePhoneDeliveryUnknown = "phoneDeliveryUnknown"

// ErrCodeRunnerStarting is POST /api/input's 503 when a touch on an iPhone
// below iOS 27 found its XCUITest runner not answering yet. Nothing was sent
// and the touch started the runner, so retrying in a few seconds is safe.
const ErrCodeRunnerStarting = "runnerStarting"

// ErrCodeRunnerUnavailable is POST /api/input's 422 when a touch on an
// iPhone below iOS 27 cannot reach a runner at all: none is built, or the
// phone is on Wi-Fi, which usbmuxd does not carry. `error` says which.
const ErrCodeRunnerUnavailable = "runnerUnavailable"

// ErrCodeHumanHeld is POST /api/input's 409 when a viewer has used the target
// within the human hold, or has a finger down on it now. Nothing was sent to
// the device. RetryAfterMs says when the hold lapses if nobody touches it
// again, and the Retry-After header says the same in whole seconds.
const ErrCodeHumanHeld = "humanHeld"

// ErrCodeClaimed is the 409 an agent gets for input on a target another agent
// has claimed, and for claiming or releasing one. `claim` names the holder.
// Nothing was sent to the device. There is no retry time: the claim lasts
// until its holder releases it or stops calling for claimLease.
const ErrCodeClaimed = "claimedBy"

// ErrCodeHandsOff is the 409 every agent gets for input on a target the
// person has marked hands off. It lasts until they clear it.
const ErrCodeHandsOff = "handsOff"

// ErrCodeNotInstalled is the launch action's 404 when the target has no app
// by that name.
const ErrCodeNotInstalled = "notInstalled"

// ErrCodeNoLaunchActivity is the launch action's 422 when the package is
// installed and has no launcher activity, so it opens only with "activity"
// named.
const ErrCodeNoLaunchActivity = "noLaunchActivity"

// ErrCodeNoSuchActivity is the launch action's 404 when the named activity
// is not one the package declares.
const ErrCodeNoSuchActivity = "noSuchActivity"

// ErrCodeNoURLHandler is the openURL action's 422 when nothing on the device
// handles the URL, such as a custom scheme whose app is not installed.
const ErrCodeNoURLHandler = "noHandler"

// ErrCodeRecording is POST /api/record's 409 when the target is already
// recording, and the 409 for fetching the MP4, a contact sheet or a DELETE
// of a recording that has not ended. Stop it first.
const ErrCodeRecording = "recording"

// ErrCodeBusy is POST /api/devices/shutdown's and DELETE /api/devices's 409
// while another shutdown or delete of the same target is still running.
const ErrCodeBusy = "busy"

// ErrCodeNotSupported is POST /api/devices/shutdown's and DELETE
// /api/devices's 422 when the target is not an AVD or simulator this server
// can act on, or is not in a state the action applies to (a shut-down AVD
// has nothing to shut down). It is also GET /api/logcat's 422 for a
// format or a since the target's log cannot give.
const ErrCodeNotSupported = "notSupported"

// ErrCodeNoFFmpeg is GET /api/record/{id}/frames's 501 when ffmpeg is not
// on PATH, in /opt/homebrew/bin or in /usr/local/bin.
const ErrCodeNoFFmpeg = "noFFmpeg"

// OKResponse is the body a handler sends when there is nothing to report but
// success -- /healthz and a successful /api/input action. Like
// ErrorResponse, this replaces an ad hoc map literal built at each call site
// so apischema has a named type to describe.
type OKResponse struct {
	OK bool `json:"ok"`
}

// ErrCodeServerDown is POST /api/servers' 502 when the peer cannot be
// reached, and the reason on the rows of a peer that stopped answering.
const ErrCodeServerDown = "serverDown"

// ErrCodeUnsupportedProtocol is POST /api/servers' 502 when the owner ends the
// stream connection with session.EndedUnsupportedProtocol: it speaks no
// version of the session protocol this server does.
const ErrCodeUnsupportedProtocol = "unsupportedProtocol"

// ErrCodeCertChanged is POST /api/servers' 502 when the peer's certificate
// does not match its pin. Removing the server and adding it
// again takes the new certificate.
const ErrCodeCertChanged = "certChanged"

// ErrCodeConfirmFingerprint is POST /api/servers' 409 when the peer's
// certificate does not verify against the system roots, or no longer matches
// the fingerprint the request carried. No token has been sent to the peer.
// Sending the request again with the answer's fingerprint pins it.
const ErrCodeConfirmFingerprint = "confirmFingerprint"
