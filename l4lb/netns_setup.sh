#!/bin/bash

cd $(dirname $0)

if [ "$EUID" -ne 0 ]; then
    echo "Please run as root"
    exit
fi

function add_ns_bare() {
    ip netns del $1
    rm /var/run/netns/$1

    ip netns add $1
    ip -n $1 l set lo up

    # disable ipv6 on the netns - for cleaner tcpdump -> enable ipv6
    ip netns exec $1 sysctl -w net.ipv6.conf.all.disable_ipv6=0
    # disable rp_filter for direct server return (DSR)
    ip netns exec $1 sysctl -w net.ipv4.conf.all.rp_filter=0
    ip netns exec $1 sysctl -w net.ipv4.conf.default.rp_filter=0
}

function add_ns() {
    ip l del $1-net0

    ip netns del $1
    rm /var/run/netns/$1

    ip netns add $1
    ip -n $1 l set lo up

    ip l add net0 netns $1 type veth peer name $1-net0
    ip -n $1 l set net0 up
    ip l set $1-net0 master brDev
    ip l set $1-net0 up

    # disable TCO - while veth optimizes the TCP transports by
    # skipping checksum computation/verification altogether,
    # we actually need a good checksum since we're going to
    # encap the packets in IPIP tunnels. Otherwise the IPIP
    # receiver would drop the packets given all its bad csums.
    ip netns exec $1 ethtool --offload net0 tx off rx off

    # disable ipv6 on the netns - for cleaner tcpdump -> enable ipv6
    ip netns exec $1 sysctl -w net.ipv6.conf.all.disable_ipv6=0
    # disable rp_filter for direct server return (DSR)
    ip netns exec $1 sysctl -w net.ipv4.conf.all.rp_filter=0
    ip netns exec $1 sysctl -w net.ipv4.conf.default.rp_filter=0
    ip netns exec $1 sysctl -w net.ipv4.conf.net0.rp_filter=0
}

function add_ip_to_ns() {
    ip -n $1 a add $2 dev net0
}

set -x
ip l add name brDev type bridge
ip l set dev brDev up

# fd6e:3de7:745b/48
# U: fd6e:3de7:745b:0/64
# VIP: fd6e:3de7:745b:1/64
# PoP: fd6e:3de7:745b:ffff/64

add_ns_bare U # "198.51.100.200/24"
add_ns R
add_ip_to_ns R "192.168.88.1/24"
add_ip_to_ns R "fd6e:3de7:745b:ffff:192:168:88:1/64"

add_ns LB
add_ip_to_ns LB "192.168.88.20/24"
add_ip_to_ns LB "fd6e:3de7:745b:ffff:192:168:88:20/64"

add_ns C0 
add_ip_to_ns C0 "192.168.88.10/24"
add_ip_to_ns C0 "fd6e:3de7:745b:ffff:192:168:88:100/64"
add_ip_to_ns C0 "fd6e:3de7:745b:ffff:192:168:88:101/64"

add_ns C1
add_ip_to_ns C1 "192.168.88.11/24"
add_ip_to_ns C1 "fd6e:3de7:745b:ffff:192:168:88:110/64"
add_ip_to_ns C1 "fd6e:3de7:745b:ffff:192:168:88:111/64"

add_ns C2
add_ip_to_ns C2 "192.168.88.12/24"
add_ip_to_ns C2 "fd6e:3de7:745b:ffff:192:168:88:120/64"
add_ip_to_ns C2 "fd6e:3de7:745b:ffff:192:168:88:121/64"

add_ns C3
add_ip_to_ns C3 "192.168.88.13/24"
add_ip_to_ns C3 "fd6e:3de7:745b:ffff:192:168:88:130/64"
add_ip_to_ns C3 "fd6e:3de7:745b:ffff:192:168:88:131/64"

#add_ns C2 "192.168.88.12/24"
#add_ns C3 "192.168.88.13/24"
add_ns O
add_ip_to_ns O "fd6e:3de7:745b:ffff:192:168:88:30/64"

for ns in R LB C0 C1 C2 C3 O; do
    ip -n "$ns" link set dev net0 mtu 1600
    ip link set dev "$ns-net0" mtu 1600
done

ip link set dev brDev mtu 1600

# veth: U (198.51.100.200/24) <-> R (198.51.100.1/24)
ip l a net0 netns U type veth peer name netU netns R
ip -n U a add 198.51.100.200/24 dev net0
ip -n U a add fd6e:3de7:745b:0000:198:51:100:200/64 dev net0
ip -n U l set net0 up
ip netns exec U ethtool --offload net0 tx off rx off
ip -n R a add 198.51.100.1/24 dev netU
ip -n R a add fd6e:3de7:745b:0000:198:51:100:1/64 dev netU
ip -n R l set netU up
ip netns exec R ethtool --offload netU tx off rx off

# enable routing on R and LB
ip netns exec R sysctl -w net.ipv4.ip_forward=1
ip netns exec LB sysctl -w net.ipv4.ip_forward=1
ip netns exec C0 sysctl -w net.ipv4.ip_forward=1
ip netns exec C1 sysctl -w net.ipv4.ip_forward=1
ip netns exec C2 sysctl -w net.ipv4.ip_forward=1
ip netns exec C3 sysctl -w net.ipv4.ip_forward=1

ip netns exec R sysctl -w net.ipv6.conf.all.forwarding=1
ip netns exec LB sysctl -w net.ipv6.conf.all.forwarding=1
ip netns exec C0 sysctl -w net.ipv6.conf.all.forwarding=1
ip netns exec C1 sysctl -w net.ipv6.conf.all.forwarding=1
ip netns exec C2 sysctl -w net.ipv6.conf.all.forwarding=1
ip netns exec C3 sysctl -w net.ipv6.conf.all.forwarding=1

# default route to R
ip -n U r add default via 198.51.100.1
ip -n LB r add default via 192.168.88.1
ip -n C0 r add default via 192.168.88.1
ip -n C1 r add default via 192.168.88.1
ip -n C2 r add default via 192.168.88.1
ip -n C3 r add default via 192.168.88.1

ip -n U r add default via fd6e:3de7:745b:0000:198:51:100:1
ip -n LB r add default via fd6e:3de7:745b:ffff:192:168:88:1
ip -n C0 r add default via fd6e:3de7:745b:ffff:192:168:88:1
ip -n C1 r add default via fd6e:3de7:745b:ffff:192:168:88:1
ip -n C2 r add default via fd6e:3de7:745b:ffff:192:168:88:1
ip -n C3 r add default via fd6e:3de7:745b:ffff:192:168:88:1

# ip netns exec O ip r add default via 192.168.88.1

# LB->C0 ipip6 / ip6ip6 tunnel - claim VIP
#ip -n C0 tunnel a ipip0 remote 192.168.88.20 local 192.168.88.10 dev net0
#ip -n C0 l set ipip0 up

ip -n C0 tunnel a ipip60 mode ipip6 remote fd6e:3de7:745b:ffff:192:168:88:20 local fd6e:3de7:745b:ffff:192:168:88:100 dev net0
ip -n C0 l set ipip60 up

ip -n C0 tunnel a ip6ip60 mode ip6ip6 remote fd6e:3de7:745b:ffff:192:168:88:20 local fd6e:3de7:745b:ffff:192:168:88:101 dev net0
ip -n C0 l set ip6ip60 up

ip -n C0 a add 192.0.2.10/32 dev ipip60
ip -n C0 a add fd6e:3de7:745b:0001:192:0:2:10/128 dev ip6ip60

# LB->C1 ipip6/ip6ip6 tunnel - claim VIP
#ip -n C1 tunnel a ipip0 remote 192.168.88.20 local 192.168.88.11 dev net0
#ip -n C1 l set ipip0 up
#ip -n C1 a add 192.0.2.10/32 dev ipip0

ip -n C1 tunnel a ipip60 mode ipip6 remote fd6e:3de7:745b:ffff:192:168:88:20 local fd6e:3de7:745b:ffff:192:168:88:110 dev net0
ip -n C1 l set ipip60 up

ip -n C1 tunnel a ip6ip60 mode ip6ip6 remote fd6e:3de7:745b:ffff:192:168:88:20 local fd6e:3de7:745b:ffff:192:168:88:111 dev net0
ip -n C1 l set ip6ip60 up

ip -n C1 a add 192.0.2.10/32 dev ipip60
ip -n C1 a add fd6e:3de7:745b:0001:192:0:2:10/128 dev ip6ip60


# LB->C2 ipip6/ip6ip6 tunnel - claim VIP
#ip -n C2 tunnel a ipip0 remote 192.168.88.20 local 192.168.88.12 dev net0
#ip -n C2 l set ipip0 up
#ip -n C2 a add 192.0.2.10/32 dev ipip0

ip -n C2 tunnel a ipip60 mode ipip6 remote fd6e:3de7:745b:ffff:192:168:88:20 local fd6e:3de7:745b:ffff:192:168:88:120 dev net0
ip -n C2 l set ipip60 up

ip -n C2 tunnel a ip6ip60 mode ip6ip6 remote fd6e:3de7:745b:ffff:192:168:88:20 local fd6e:3de7:745b:ffff:192:168:88:121 dev net0
ip -n C2 l set ip6ip60 up

ip -n C2 a add 192.0.2.10/32 dev ipip60
ip -n C2 a add fd6e:3de7:745b:0001:192:0:2:10/128 dev ip6ip60

# LB->C3 ipip6/ip6ip6 tunnel - claim VIP
#ip -n C3 tunnel a ipip0 remote 192.168.88.20 local 192.168.88.13 dev net0
#ip -n C3 l set ipip0 up
#ip -n C3 a add 192.0.2.10/32 dev ipip0

ip -n C3 tunnel a ipip60 mode ipip6 remote fd6e:3de7:745b:ffff:192:168:88:20 local fd6e:3de7:745b:ffff:192:168:88:130 dev net0
ip -n C3 l set ipip60 up

ip -n C3 tunnel a ip6ip60 mode ip6ip6 remote fd6e:3de7:745b:ffff:192:168:88:20 local fd6e:3de7:745b:ffff:192:168:88:131 dev net0
ip -n C3 l set ip6ip60 up

ip -n C3 a add 192.0.2.10/32 dev ipip60
ip -n C3 a add fd6e:3de7:745b:0001:192:0:2:10/128 dev ip6ip60

# Route VIPs to LB
ip -n R r add 192.0.2.0/24 via 192.168.88.20
ip -n R r add fd6e:3de7:745b:0001:192:0:2:0/112 via fd6e:3de7:745b:ffff:192:168:88:20

# inject dummy xdp prog to workaround https://www.spinics.net/lists/netdev/msg625217.html
(cd c && make dummy.o)
ip l set dev LB-net0 xdp obj c/dummy.o sec xdp
