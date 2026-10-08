"""Send one IGMPv2 group-specific membership query: igmp-query.py <source-address> <group>.

A member of <group> on the segment answers with a membership report within a second (the max
response time below), and a host that has not joined it says nothing. Which is what makes this a
probe of inbound multicast that needs no listener of ours: the answer comes from the member's
KERNEL, and the query only reaches that kernel if every filter on the way in passed a frame
addressed to the group's MAC.

Standard library only. The kernel writes the IP header (TTL 1 is the multicast default); the
router-alert option is omitted, and Linux answers a query without it.
"""

import socket
import struct
import sys

source, group = sys.argv[1], sys.argv[2]


def checksum(data):
    total = sum(struct.unpack("!%dH" % (len(data) // 2), data))
    total = (total >> 16) + (total & 0xFFFF)
    total += total >> 16
    return ~total & 0xFFFF


# type 0x11 (membership query), max response time 10 tenths of a second -- non-zero, or a Linux
# receiver reads it as an IGMPv1 query -- and the group itself, which makes it group-specific.
body = struct.pack("!BBH4s", 0x11, 10, 0, socket.inet_aton(group))
body = struct.pack("!BBH4s", 0x11, 10, checksum(body), socket.inet_aton(group))

s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_IGMP)
s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF, socket.inet_aton(source))
s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_LOOP, 0)
s.sendto(body, (group, 0))
