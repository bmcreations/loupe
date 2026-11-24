package target

// Turned returns the frame rotated a quarter turn clockwise, quarters times,
// carrying the count so the client can turn the art the same way.
//
// Only the geometry moves here. The art keeps its filename and its own pixel
// grid, because rotating a webp server-side would mean re-encoding it and the
// browser already turns an image for free.
func (f *DeviceFrame) Turned(quarters int) *DeviceFrame {
	if f == nil {
		return nil
	}
	quarters = ((quarters % 4) + 4) % 4
	out := *f
	for i := 0; i < quarters; i++ {
		d := out.Display
		out.Display = FrameRect{
			X:      out.Height - (d.Y + d.Height),
			Y:      d.X,
			Width:  d.Height,
			Height: d.Width,
		}
		out.Width, out.Height = out.Height, out.Width
	}
	out.Rotation = quarters
	return &out
}
