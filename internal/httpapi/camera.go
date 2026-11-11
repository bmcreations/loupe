package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg"
	pngcodec "image/png"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/bmcreations/loupe/internal/camfeed"
	"github.com/bmcreations/loupe/internal/target"
)

// maxCameraBytes bounds a camera image upload. A var so a test can shrink it.
var maxCameraBytes int64 = 32 << 20

// maxCameraSide bounds each side of a camera image, checked from the header
// before the pixels are decoded.
const maxCameraSide = 8192

// cameraTimeout covers building the simulator shim on first use.
const cameraTimeout = 2 * time.Minute

// CameraHostRequest is the JSON body of POST /api/camera that stages one of
// the Mac's cameras instead of an image.
type CameraHostRequest struct {
	// HostCamera is a camera name as GET /api/camera/sources lists it.
	HostCamera string `json:"hostCamera"`
}

// CameraSourcesResponse is what GET /api/camera/sources answers.
type CameraSourcesResponse struct {
	Target  string              `json:"target"`
	Cameras []target.HostCamera `json:"cameras"`
}

// CameraResponse is what POST and DELETE /api/camera answer on success.
type CameraResponse struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	// Applies is when the app looking through the camera sees the change:
	// cameraOpen, boot, live or launch.
	Applies target.CameraApplies `json:"applies"`
	// Live is true when the frame went to the Handset Camera feed rather than
	// being staged as a still. It lets a client streaming frames tell "fed the
	// live camera" from "staged a PNG" when both report applies: boot.
	Live bool `json:"live,omitempty"`
	// HostCamera is the Mac camera a POST staged, when it staged one.
	HostCamera string `json:"hostCamera,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	TookMs     int64  `json:"tookMs"`
}

// handleCamera points a device's back camera at an image (POST, the body a
// PNG or JPEG, raw; or, on an AVD, JSON {"hostCamera": "<name>"} to use one
// of the Mac's cameras) or gives the camera back (DELETE). An AVD takes it as a
// boot flag and reads it on each camera open; a simulator app takes it
// through the camera shim loupe launches it with. The image is always passed
// on as PNG: the emulator reads its file's type from the extension.
//
// Like /api/media it is not arbitrated: it changes no screen by itself.
func (s *Server) handleCamera(w http.ResponseWriter, r *http.Request) {
	began := time.Now()
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	cf, ok := t.(target.CameraFeeder)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no camera loupe can feed: AVDs and iOS simulators only (docs/agent-api.md, /api/camera)", t.ID()),
		})
		return
	}
	s.awake.Touch(agentAwake)

	rec := map[string]any{"kind": "agent", "target": t.ID(), "action": "camera", "method": r.Method, "ua": r.UserAgent()}
	finish := func(outcome string, err error) {
		rec["outcome"], rec["tookMs"] = outcome, time.Since(began).Milliseconds()
		if err != nil {
			rec["err"] = err.Error()
		}
		s.trace.Record(rec)
	}
	ctx, cancel := context.WithTimeout(r.Context(), cameraTimeout)
	defer cancel()

	if r.Method == http.MethodDelete {
		applies, err := cf.ClearCamera(ctx)
		if err != nil {
			finish("error", err)
			writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
			return
		}
		rec["applies"] = applies
		finish("cleared", nil)
		writeJSON(w, http.StatusOK, CameraResponse{Target: t.ID(), OK: true, Applies: applies, TookMs: time.Since(began).Milliseconds()})
		return
	}

	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/json" {
		s.stageHostCamera(w, r, t, rec, finish, ctx, began)
		return
	}

	raw, format, size, status, err := readCameraImage(http.MaxBytesReader(w, r.Body, maxCameraBytes))
	if err != nil {
		finish("refused", err)
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	rec["width"], rec["height"], rec["bytes"] = size.X, size.Y, len(raw)
	// An AVD on Handset Camera takes the frame as sent, with no PNG round trip:
	// a client streaming its webcam sends several a second.
	if lf, ok := t.(target.LiveCameraFeeder); ok {
		applies, handled, err := lf.FeedCameraFrame(ctx, raw)
		if handled {
			rec["live"] = true
			if err != nil {
				status := http.StatusBadGateway
				if errors.Is(err, camfeed.ErrNoCamera) || errors.Is(err, camfeed.ErrNoHelper) || errors.Is(err, camfeed.ErrBusy) {
					status = http.StatusServiceUnavailable
				}
				finish("error", err)
				writeJSON(w, status, ErrorResponse{Error: err.Error()})
				return
			}
			rec["applies"] = applies
			finish("ok", nil)
			writeJSON(w, http.StatusOK, CameraResponse{
				Target: t.ID(), OK: true, Applies: applies, Live: true, HostCamera: camfeed.CameraName(),
				Width: size.X, Height: size.Y, TookMs: time.Since(began).Milliseconds(),
			})
			return
		}
	}
	pngBytes, status, err := cameraPNG(raw, format)
	if err != nil {
		finish("refused", err)
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	applies, err := cf.SetCamera(ctx, pngBytes)
	if err != nil {
		finish("error", err)
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	rec["applies"] = applies
	finish("ok", nil)
	writeJSON(w, http.StatusOK, CameraResponse{
		Target: t.ID(), OK: true, Applies: applies, Width: size.X, Height: size.Y,
		TookMs: time.Since(began).Milliseconds(),
	})
}

// stageHostCamera answers a POST /api/camera whose body is JSON: stage one of
// the Mac's cameras. Only AVDs can take one.
func (s *Server) stageHostCamera(w http.ResponseWriter, r *http.Request, t target.Target, rec map[string]any, finish func(string, error), ctx context.Context, began time.Time) {
	hf, ok := t.(target.HostCameraFeeder)
	if !ok {
		err := fmt.Errorf("target %q cannot take a host camera: AVDs only; send an image instead (docs/agent-api.md, /api/camera)", t.ID())
		finish("refused", err)
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: err.Error()})
		return
	}
	var req CameraHostRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.HostCamera == "" {
		err = errors.New(`body must be {"hostCamera": "<name>"}; GET /api/camera/sources lists the names`)
		finish("refused", err)
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	rec["hostCamera"] = req.HostCamera
	applies, err := hf.SetHostCamera(ctx, req.HostCamera)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, target.ErrNoHostCamera) {
			status = http.StatusNotFound
		}
		finish("error", err)
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	rec["applies"] = applies
	finish("ok", nil)
	writeJSON(w, http.StatusOK, CameraResponse{
		Target: t.ID(), OK: true, Applies: applies, HostCamera: req.HostCamera,
		TookMs: time.Since(began).Milliseconds(),
	})
}

// handleCameraFeed stops the live feed of an AVD on Handset Camera, DELETE
// /api/camera/feed?target=<avd>: the camera shows its placeholder at once
// instead of repeating the last frame, and stays staged, so frames posted
// later play with no restart. DELETE /api/camera also unstages it.
func (s *Server) handleCameraFeed(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	lf, ok := t.(target.LiveCameraFeeder)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q has no live camera: AVDs on Handset Camera only (docs/agent-api.md, /api/camera/feed)", t.ID()),
		})
		return
	}
	s.awake.Touch(agentAwake)
	if !lf.StopCameraFeed() {
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Error: fmt.Sprintf("target %q is not on Handset Camera; DELETE /api/camera gives the camera back", t.ID()),
		})
		return
	}
	s.trace.Record(map[string]any{"kind": "agent", "target": t.ID(), "action": "camera-feed", "method": r.Method, "outcome": "stopped", "ua": r.UserAgent()})
	writeJSON(w, http.StatusOK, CameraResponse{Target: t.ID(), OK: true, Applies: target.CameraLive, Live: true, HostCamera: camfeed.CameraName()})
}

// handleCameraSources lists the Mac cameras an AVD can take as its back
// camera, GET /api/camera/sources?target=<avd>.
func (s *Server) handleCameraSources(w http.ResponseWriter, r *http.Request) {
	t, ok := s.agentTarget(w, r)
	if !ok {
		return
	}
	hf, ok := t.(target.HostCameraFeeder)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{
			Error: fmt.Sprintf("target %q cannot take a host camera: AVDs only (docs/agent-api.md, /api/camera/sources)", t.ID()),
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), cameraTimeout)
	defer cancel()
	cams, err := hf.HostCameras(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	if cams == nil {
		cams = []target.HostCamera{}
	}
	writeJSON(w, http.StatusOK, CameraSourcesResponse{Target: t.ID(), Cameras: cams})
}

// readCameraImage reads a PNG or JPEG body and checks its size from the
// header. On error it returns the status to answer with.
func readCameraImage(body io.Reader) ([]byte, string, image.Point, int, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, "", image.Point{}, http.StatusRequestEntityTooLarge, fmt.Errorf("image is over %d bytes", maxCameraBytes)
		}
		return nil, "", image.Point{}, http.StatusBadRequest, err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") {
		return nil, "", image.Point{}, http.StatusBadRequest, errors.New("body is not a PNG or JPEG image")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxCameraSide || cfg.Height > maxCameraSide {
		return nil, "", image.Point{}, http.StatusBadRequest, fmt.Errorf("image is %dx%d; each side must be 1 to %d pixels", cfg.Width, cfg.Height, maxCameraSide)
	}
	return raw, format, image.Pt(cfg.Width, cfg.Height), 0, nil
}

// cameraPNG returns an image readCameraImage accepted as PNG. A PNG goes
// through unchanged once it decodes; a JPEG is decoded and re-encoded. On
// error it returns the status to answer with.
func cameraPNG(raw []byte, format string) ([]byte, int, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("decode %s: %w", format, err)
	}
	if format == "png" {
		return raw, 0, nil
	}
	// A JPEG decodes as YCbCr, which the PNG encoder would write at 16 bits
	// a channel: twice the bytes for nothing a camera frame can show.
	rgba := image.NewNRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	var out bytes.Buffer
	if err := pngcodec.Encode(&out, rgba); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return out.Bytes(), 0, nil
}
