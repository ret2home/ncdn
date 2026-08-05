package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yzp0n/ncdn/l4lb/l4lbdrv"
)

var lbBin = flag.String("lbBin", "c/lb.o", "Path to XDP lb binary")
var xdpcapHookPath = flag.String("xdpcapHookPath", "/sys/fs/bpf/xdpcap_hook", "Path to XDPCap hook")
var xdpif = flag.String("interface", "net0", "Interface to attach lb prog to")
var vip4 = flag.String("vip4", "192.0.2.10", "VIP address to load balance")
var vip6 = flag.String("vip6", "fd6e:3de7:745b:0001:192:0:2:10", "VIP address to load balance")
var dest_ipip6str = flag.String("dests_ipip6", "", "Comma separated list of destination IP and MAC addresses. (Example: 192.168.88.10;00:00:5e:00:53:01,)")
var dest_ip6ip6str = flag.String("dests_ip6ip6", "", "Comma separated list of destination IP and MAC addresses. (Example: fd6e:3de7:745b:ffff:192:168:88:100;00:00:5e:00:53:01,)")
var statusz = flag.String("statusz", ":8889/statusz", "health check dest")

func parseDest(deststr string) ([]l4lbdrv.DestinationEntry, error) {
	commas := strings.Split(deststr, ",")
	dests := make([]l4lbdrv.DestinationEntry, 0, len(commas))
	for _, c := range commas {
		if c == "" {
			continue
		}

		parts := strings.Split(c, ";")
		if len(parts) != 2 {
			return nil, fmt.Errorf("Invalid destination entry: %s", c)
		}
		ip6 := netip.MustParseAddr(parts[0])
		if ip6.Is4() {
			return nil, fmt.Errorf("Destination must be ipv6 address, but was %s", ip6)
		}

		mac, err := net.ParseMAC(parts[1])
		if err != nil {
			return nil, fmt.Errorf("Invalid MAC address: %s", parts[1])
		}

		dests = append(dests, l4lbdrv.DestinationEntry{
			IPAddr:       ip6,
			HardwareAddr: mac,
			IsAlive:      1,
		})
	}
	log.Printf("dests: %+v", dests)
	return dests, nil
}

func main() {
	flag.Parse()

	dests_ipip6, err := parseDest(*dest_ipip6str)
	if err != nil {
		slog.Error("Failed to parse dest string", slog.String("err", err.Error()))
	}
	dests_ip6ip6, err := parseDest(*dest_ip6ip6str)
	if err != nil {
		slog.Error("Failed to parse dest string", slog.String("err", err.Error()))
	}

	cfg := &l4lbdrv.Config{
		BinPath:         *lbBin,
		XdpCapHookPath:  *xdpcapHookPath,
		InterfaceName:   *xdpif,
		VIP4:            netip.MustParseAddr(*vip4),
		VIP6:            netip.MustParseAddr(*vip6),
		DestsIpIp6:      dests_ipip6,
		DestsIp6Ip6:     dests_ip6ip6,
		HealthCheckDest: *statusz,
		SlotsLength:     4096,
	}
	lb, err := l4lbdrv.New(cfg)
	if err != nil {
		log.Panicf("Failed to create l4lb instance: %v", err)
	}
	slog.Info("L4LB started.")
	defer lb.Close()

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(time.Second)
	for {
		select {
		case <-ticker.C:
			if err := lb.DumpCounters(); err != nil {
				slog.Error("Failed to dump counters", slog.String("err", err.Error()))
			}
			changed := lb.DoHealthCheck()
			if changed {
				slog.Info("sync!")
				if err := lb.Sync(); err != nil {
					slog.Error("Failed to sync maps", slog.String("err", err.Error()))
				}
			}
			continue

		case <-done:
			break
		}
		break
	}
	slog.Info("Shutting down.")
}
