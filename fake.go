package localhost

// Fake is a module for tests elsewhere: it reads the machine under its root option, as a
// fixture, with every filesystem 100 GiB and 40 used, and no services or journal.
func Fake() *Module {
	m := New()
	m.statfs = func(string) (fsUsage, error) { return fsUsage{total: 100 << 30, used: 40 << 30, avail: 60 << 30}, nil }
	m.addrs, m.system = nil, nil
	return m
}
