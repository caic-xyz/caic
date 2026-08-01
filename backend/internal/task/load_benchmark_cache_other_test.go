// Reports unsupported cold-cache task adoption benchmarks on non-Linux systems.
//go:build adoption_benchmark && !linux

package task

func prepareColdAdoptionFixture(string) error {
	return errAdoptionColdUnsupported
}
