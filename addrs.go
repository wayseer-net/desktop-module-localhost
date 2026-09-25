package localhost

import "net"

// interfaceAddrs maps each interface to its addresses, without prefix lengths.
func interfaceAddrs() map[string][]string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, i := range ifs {
		as, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range as {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLinkLocalUnicast() {
				out[i.Name] = append(out[i.Name], n.IP.String())
			}
		}
	}
	return out
}
