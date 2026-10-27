// Picture in Picture: the stream's canvas fed to a <video> through
// captureStream(), so Safari can float it over other apps.
//
// A spike, behind ?pip=1. The question it exists to answer is whether the
// WebCodecs decoder keeps running once the page is hidden behind a PiP
// window; iOS fails the decoder as it hides a page (see app.js's
// decode-error), and if that still happens here the window shows one frozen
// frame and nothing else in this file matters.
//
// The video is created and playing before anyone asks for PiP, because
// requestPictureInPicture() refuses a video with no metadata, and a
// captureStream video only has metadata once a frame has been drawn. The
// request itself then runs inside the menu tap, which Safari requires.
//
// It sits 1px and transparent rather than display:none: Safari neither plays
// nor floats a video that is not rendered. playsinline keeps play() from
// going full screen on an iPhone.
//
// deps:
//   report(obj) -- client telemetry.
//   onChange(active) -- PiP entered or left, so app.js can keep the stream
//     open while hidden and paint without rAF.

export function mount(canvas, deps) {
  const video = document.createElement('video');
  video.muted = true;
  video.autoplay = true;
  video.playsInline = true;
  video.setAttribute('playsinline', '');
  video.setAttribute('aria-hidden', 'true');
  Object.assign(video.style, {
    position: 'fixed', left: '0', top: '0', width: '1px', height: '1px',
    opacity: '0', pointerEvents: 'none',
  });

  // Which API this Safari takes is checked rather than assumed, and reported.
  const standard = typeof video.requestPictureInPicture === 'function'
    && document.pictureInPictureEnabled !== false;
  const webkit = typeof video.webkitSupportsPresentationMode === 'function'
    && video.webkitSupportsPresentationMode('picture-in-picture');
  const supported = !!(canvas.captureStream && (standard || webkit));

  let attached = false;
  let active = false;
  let startedAt = 0;

  function attach() {
    if (attached || !supported) return;
    attached = true;
    document.body.appendChild(video);
    video.srcObject = canvas.captureStream();
    video.play().catch((err) => deps.report({ event: 'pip-error', stage: 'play', message: String(err && err.message || err) }));
  }

  function set(on, via) {
    if (on === active) return;
    active = on;
    if (on) startedAt = performance.now();
    deps.report({
      event: on ? 'pip-start' : 'pip-stop',
      api: via,
      hidden: document.hidden,
      ...(on ? {} : { forMs: Math.round(performance.now() - startedAt) }),
    });
    deps.onChange(on);
  }

  // On iOS 27.2 one decode-error ("Decoder failure", visible, no suspend
  // after it) followed a pip-stop by 1.3s. The decoder's error path rebuilds
  // and waits for the next keyframe, so nothing here handles it; why leaving
  // PiP fails the decoder is not known. The run's other decoder failure
  // came 128ms before a suspend, so both sat next to a change in what the
  // system was showing.
  video.addEventListener('enterpictureinpicture', () => set(true, 'standard'));
  video.addEventListener('leavepictureinpicture', () => set(false, 'standard'));
  video.addEventListener('webkitpresentationmodechanged', () => {
    set(video.webkitPresentationMode === 'picture-in-picture', 'webkit');
  });

  // Called inside the tap. Neither branch may await before its request.
  function toggle() {
    attach();
    if (active) {
      if (standard && document.pictureInPictureElement) return document.exitPictureInPicture();
      if (webkit) return video.webkitSetPresentationMode('inline');
      return;
    }
    deps.report({
      event: 'pip-request',
      readyState: video.readyState,
      w: video.videoWidth, h: video.videoHeight,
      api: standard ? 'standard' : 'webkit',
    });
    if (standard) {
      return video.requestPictureInPicture().catch((err) => {
        deps.report({ event: 'pip-error', stage: 'request', api: 'standard', message: String(err && err.message || err) });
        throw err;
      });
    }
    video.webkitSetPresentationMode('picture-in-picture');
  }

  // retarget follows focus to another pane's canvas. A floating window
  // keeps playing: only its stream changes.
  function retarget(next) {
    if (!next || next === canvas) return;
    canvas = next;
    if (attached) video.srcObject = canvas.captureStream();
  }

  return {
    supported,
    attach,
    retarget,
    toggle,
    active: () => active,
    label: () => (active ? 'Leave picture in picture' : 'Picture in picture'),
  };
}
