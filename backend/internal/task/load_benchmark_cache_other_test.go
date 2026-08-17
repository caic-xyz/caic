// Reports unsupported cold-cache task adoption benchmarks on non-Linux systems.
//go:build !linux

package task

func prepareColdAdoptionFixture(string) error {
	return errAdoptionColdUnsupported
}
