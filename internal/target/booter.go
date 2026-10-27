package target

// Booter reports whether opening a target will trigger a boot rather than
// join something already running. It is optional -- implemented only by
// Emulator -- because a physical device or the synthetic target never boots
// anything on Open, and a target that cannot say so is not a Booter at all
// rather than one that always answers false.
type Booter interface {
	BootsOnOpen() bool
}

// BootsOnOpen reports true until ensureBooted has recorded this AVD as
// attached. It is not "is this a registered AVD" -- every Emulator is one --
// it is "will Open have to wait out a boot", which is exactly what has not
// happened yet the first time a caller asks and never again afterwards: an
// Emulator does not un-attach once it has booted, since Shutdown stops only a
// process this target itself started, and Describe's own comment notes the
// registry never re-enumerates a target once it exists.
func (e *Emulator) BootsOnOpen() bool { return !e.attached.Load() }
