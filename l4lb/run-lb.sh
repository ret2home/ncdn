#!/bin/bash
set -e

export MY_USER=${USER}
export SRC_DIR=$(readlink -f $(dirname $0)/..)
export BIN_DIR=/tmp/ncdn-bin
mkdir -p ${BIN_DIR}

set -x
(cd ${SRC_DIR}/l4lb/c && make)
go build -o ${BIN_DIR}/l4lb ${SRC_DIR}/l4lb/cmd
set +x

cd ${SRC_DIR}/l4lb


mapfile -t ip6s < <(
    sudo ip netns exec LB ip -json -f inet6 a show net0 |
    jq -r '.[].addr_info[].local | select(startswith("fd6e:"))' |
    sort -t: -k8,8
)

mac_lb=$(sudo ip netns exec LB cat /sys/class/net/net0/address)

dests_ipip6="${ip6s[0]};${mac_lb},"
dests_ip6ip6="${ip6s[0]};${mac_lb},"

for ns in C0 C1 C2; do
    mapfile -t ip6s < <(
        sudo ip netns exec "$ns" ip -json -f inet6 a show net0 |
        jq -r '.[].addr_info[].local | select(startswith("fd6e:"))' |
        sort -t: -k8,8
    )

    mac=$(sudo ip netns exec "$ns" cat /sys/class/net/net0/address)

    dests_ipip6="${dests_ipip6}${ip6s[0]};${mac},"
    dests_ip6ip6="${dests_ip6ip6}${ip6s[1]};${mac},"
done

echo ${dests_ipip6}
echo ${dests_ip6ip6}

sudo ip -n LB tunn del ipip0 || echo "no ipip0. good" # in case it exists from a `nolb.sh` run
sudo ip netns exec LB ${BIN_DIR}/l4lb -xdpcapHookPath="" -dests_ipip6="${dests_ipip6}" -dests_ip6ip6="${dests_ip6ip6}"
