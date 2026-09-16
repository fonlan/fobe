package agent

import "errors"

var errICMPUnimplemented = errors.New("icmp echo unavailable (needs raw socket / CAP_NET_RAW or ping_group_range)")
