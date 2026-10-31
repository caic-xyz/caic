// Reports unsupported cold-cache task adoption benchmarks on non-Linux systems.
//go:build !linux

package history

func prepareColdAdoptionFixture(string) error {
	return errAdoptionColdUnsupported
}
