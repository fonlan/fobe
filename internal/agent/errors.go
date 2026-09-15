package agent

import "errors"

var errICMPUnimplemented = errors.New("icmp echo unavailable on this platform (needs linux + raw socket)")
