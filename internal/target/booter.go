package target

// Booter reports whether opening a target will trigger a boot rather than
// join something already running. It is optional -- implemented only by
// Emulator -- because a physical device or the synthetic target never boots
// anything on Open, and a target that cannot say so is not a Booter at all
// rather than one that always answers false.
type Booter interface {
	BootsOnOpen() bool
}
