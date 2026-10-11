package config

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Memory selects local buckets or a replicated Raft state machine.
type Memory struct {
	Distributed Distributed `yaml:"distributed"`
}

// Distributed configures a fixed Raft cluster using each member's admin listener.
type Distributed struct {
	Enabled        bool          `yaml:"enabled"`
	Address        string        `yaml:"address"`
	Peers          []string      `yaml:"peers"`
	DataDir        string        `yaml:"data_dir"`
	Token          string        `yaml:"token"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// Validate checks peer identities and storage settings without exposing secrets.
func (d Distributed) Validate() error {
	validAddress := func(raw string) bool {
		address, err := netip.ParseAddrPort(raw)
		return err == nil && address.Port() > 0 && !address.Addr().IsUnspecified() &&
			!address.Addr().IsMulticast() && !address.Addr().Is4In6() && address.Addr().Zone() == "" && address.String() == raw
	}
	if !validAddress(d.Address) {
		return fmt.Errorf("rate_limit.memory.distributed.address must be a canonical IP:admin-port address")
	}

	seen := make(map[string]bool, len(d.Peers))
	for _, peer := range d.Peers {
		if !validAddress(peer) || seen[peer] {
			return fmt.Errorf("rate_limit.memory.distributed.peers must contain unique canonical IP:admin-port addresses")
		}
		seen[peer] = true
	}
	if !seen[d.Address] {
		return fmt.Errorf("rate_limit.memory.distributed.peers must include rate_limit.memory.distributed.address")
	}

	if strings.TrimSpace(d.DataDir) == "" {
		return fmt.Errorf("rate_limit.memory.distributed.data_dir must not be empty")
	}
	if d.Token == "" || strings.IndexFunc(d.Token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return fmt.Errorf("rate_limit.memory.distributed.token must contain nonempty printable ASCII without spaces")
	}

	if d.RequestTimeout <= 0 {
		return fmt.Errorf("rate_limit.memory.distributed.request_timeout must be positive")
	}
	return nil
}
