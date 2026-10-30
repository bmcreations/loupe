package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg"
	pngcodec "image/png"
	"io"
	"net/http"
	"time"

	"github.com/bmcreations/loupe/internal/target"
)

// maxCameraBytes bounds a camera image upload. A var so a test can shrink it.
var maxCameraBytes int64 = 32 << 20

// maxCameraSide bounds each side of a camera image, checked from the header
// before the pixels are decoded.
const maxCameraSide = 8192

// cameraTimeout covers building the simulator shim on first use.
const cameraTimeout = 2 * time.Minute

// CameraResponse is what POST and DELETE /api/camera answer on success.
type CameraResponse struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	// Applies is when the app looking through the camera sees the change:
	// cameraOpen, boot, live or launch.
	Applies target.CameraApplies `json:"applies"`
	Width   int                  `json:"width,omitempty"`
	Height  int                  `json:"height,omitempty"`
	TookMs  int64                `json:"tookMs"`
}

// handleCamera points a device's back camera at an image (POST, the body a
// PNG or JPEG, raw) or gives the camera back (DELETE). An AVD takes it as a
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

	pngBytes, size, status, err := cameraPNG(http.MaxBytesReader(w, r.Body, maxCameraBytes))
	if err != nil {
		finish("refused", err)
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	rec["width"], rec["height"], rec["bytes"] = size.X, size.Y, len(pngBytes)
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

// cameraPNG reads a PNG or JPEG body and returns it as PNG with its size. A
// PNG goes through unchanged; a JPEG is decoded and re-encoded. On error it
// returns the status to answer with.
func cameraPNG(body io.Reader) ([]byte, image.Point, int, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, image.Point{}, http.StatusRequestEntityTooLarge, fmt.Errorf("image is over %d bytes", maxCameraBytes)
		}
		return nil, image.Point{}, http.StatusBadRequest, err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") {
		return nil, image.Point{}, http.StatusBadRequest, errors.New("body is not a PNG or JPEG image")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxCameraSide || cfg.Height > maxCameraSide {
		return nil, image.Point{}, http.StatusBadRequest, fmt.Errorf("image is %dx%d; each side must be 1 to %d pixels", cfg.Width, cfg.Height, maxCameraSide)
	}
	size := image.Pt(cfg.Width, cfg.Height)
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, image.Point{}, http.StatusBadRequest, fmt.Errorf("decode %s: %w", format, err)
	}
	if format == "png" {
		return raw, size, 0, nil
	}
	// A JPEG decodes as YCbCr, which the PNG encoder would write at 16 bits
	// a channel: twice the bytes for nothing a camera frame can show.
	rgba := image.NewNRGBA(img.Bounds())
	draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
	var out bytes.Buffer
	if err := pngcodec.Encode(&out, rgba); err != nil {
		return nil, image.Point{}, http.StatusInternalServerError, err
	}
	return out.Bytes(), size, 0, nil
}
