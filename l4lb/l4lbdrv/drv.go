package l4lbdrv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"go.uber.org/multierr"
)

type Config struct {
	BinPath        string
	InterfaceName  string
	XdpCapHookPath string

	VIP             netip.Addr
	Dests           DestinationEntries
	HealthCheckDest string
	SlotsLength     int
}
type L4LB struct {
	cfg           *Config
	backendStatus []int // 0,1 -> DOWN, 2,3 -> LIVE
	bindings      *Bindings
	linkAttacher  *LinkAttacher
}

func New(cfg *Config) (*L4LB, error) {
	if err := PrepSystemForXDP(); err != nil {
		return nil, fmt.Errorf("Failed to prep system for XDP: %w", err)
	}
	aBinPath, err := filepath.Abs(cfg.BinPath)
	if err != nil {
		return nil, fmt.Errorf("Failed to get absolute path for %s: %w", cfg.BinPath, err)
	}
	var aXdpcapHookPath string
	if cfg.XdpCapHookPath != "" {
		aXdpcapHookPath, err = filepath.Abs(cfg.XdpCapHookPath)
		if err != nil {
			return nil, fmt.Errorf("Failed to get absolute path for %s: %w", cfg.XdpCapHookPath, err)
		}
	}

	bindings, err := BindBalancer(aBinPath, aXdpcapHookPath)
	if err != nil {
		return nil, fmt.Errorf("Failed to bind balancer: %w", err)
	}

	lb := &L4LB{
		cfg:      cfg,
		bindings: bindings,
	}

	var link netlink.Link
	if cfg.InterfaceName == "" {
		slog.Info("No interface name provided, skipping link attachment.")
	} else {
		l, err := netlink.LinkByName(cfg.InterfaceName)
		if err != nil {
			return nil, fmt.Errorf("Failed to find interface %q: %w", cfg.InterfaceName, err)
		}
		link = l
	}
	if link != nil {
		a, err := AttachToLink(link, bindings.LBMain.FD())
		if err != nil {
			return nil, multierr.Combine(err, bindings.Close())
		}
		lb.linkAttacher = a
	}
	lb.backendStatus = make([]int, len(cfg.Dests))
	for i := 0; i < len(cfg.Dests); i++ {
		lb.backendStatus[i] = 3 // weakly-live
	}

	if err := lb.Sync(); err != nil {
		return nil, fmt.Errorf("Initial map sync failed: %w", err)
	}

	return lb, nil
}

var hostOrder = binary.LittleEndian

func IPToUint32(ip netip.Addr) (uint32, error) {
	if !ip.Is4() {
		return 0, errors.New("Given IP is not an IPv4 address.")
	}

	ip4 := ip.As4()
	return hostOrder.Uint32(ip4[:]), nil
}

func calcHash(serverID uint32, slotID uint32) uint64 {
	var key [8]byte
	binary.LittleEndian.PutUint32(key[0:4], slotID)
	binary.LittleEndian.PutUint32(key[4:8], serverID)
	return xxhash.Sum64(key[:])
}
func selectPop(status []int, slotId uint32) uint32 {
	var maxId uint32 = 0
	var maxHash uint64 = 0
	for i := 1; i < len(status); i++ {
		if status[i] >= 3 {
			hs := calcHash(uint32(i), slotId)
			if maxHash < hs {
				maxHash = hs
				maxId = uint32(i)
			}
		}
	}
	if maxId == 0 {
		return uint32(rand.Int32N(int32(len(status)-1)) + 1)
	}
	return maxId
}

func (lb *L4LB) Sync() error {
	vip4, err := IPToUint32(lb.cfg.VIP)
	if err != nil {
		return fmt.Errorf("vip: %w", err)
	}

	err = lb.bindings.ConfigMap.Update(uint32(0), &LbConfig{
		VipAddress: vip4,
		NumDests:   uint32(len(lb.cfg.Dests) - 1),
	}, 0)
	if err != nil {
		return fmt.Errorf("Failed to update ConfigMap: %w", err)
	}

	keys := make([]uint32, len(lb.cfg.Dests))
	for i := range keys {
		keys[i] = uint32(i)
	}

	_, err = lb.bindings.DestinationArray.BatchUpdate(keys, lb.cfg.Dests, &ebpf.BatchOptions{})
	if err != nil {
		return fmt.Errorf("Failed to update DestinationArray: %w", err)
	}

	slotIds := make([]uint32, lb.cfg.SlotsLength)
	for i := range slotIds {
		slotIds[i] = uint32(i)
	}
	destIdForSlots := make([]uint32, lb.cfg.SlotsLength)
	for i := range destIdForSlots {
		destIdForSlots[i] = selectPop(lb.backendStatus, uint32(i))
	}

	fmt.Printf("changed! %v\n", lb.backendStatus)

	_, err = lb.bindings.SlotsArray.BatchUpdate(slotIds, destIdForSlots, &ebpf.BatchOptions{})
	if err != nil {
		return fmt.Errorf("Failed to update slots map: %w", err)
	}

	return nil
}

func (lb *L4LB) Close() error {
	return lb.bindings.Close()
}

func (lb *L4LB) DumpCounters() error {
	cnt, err := lb.bindings.ReadStatCountersAggregate()
	if err != nil {
		return err
	}

	slog.Info(cnt.String())

	return nil
}

// `PrepSystemForXDP` configures RLIMIT_MEMLOCK to ensure enough room to
// allocate eBPF programs and maps on older Linux systems.
func PrepSystemForXDP() error {
	const RLIMIT_MEMLOCK = 8
	var rlim syscall.Rlimit
	if err := syscall.Getrlimit(RLIMIT_MEMLOCK, &rlim); err != nil {
		return fmt.Errorf("Failed to Getrlimit(RLIMIT_MEMLOCK): %v", err)
	}
	slog.Info("Getrlimit(RLIMIT_MEMLOCK)", "Cur", rlim.Cur, "Max", rlim.Max)

	rlim.Cur = math.MaxUint64
	rlim.Max = math.MaxUint64
	if err := syscall.Setrlimit(RLIMIT_MEMLOCK, &rlim); err != nil {
		return fmt.Errorf("Failed to Setrlimit(RLIMIT_MEMLOCK): %v", err)
	}
	slog.Info("Setrlimit(RLIMIT_MEMLOCK)", "Cur", rlim.Cur, "Max", rlim.Max)

	return nil
}

func HealthCheckSingle(url string) bool {
	client := &http.Client{
		Timeout: 300 * time.Millisecond,
	}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	success := resp.StatusCode == http.StatusOK
	if resp != nil {
		resp.Body.Close()
	}
	return success
}

func NewBackendStatus(status int, result bool) int {
	if result {
		return min(3, status+1)
	}
	return max(0, status-1)
}
func (lb *L4LB) DoHealthCheck() bool {
	lens := len(lb.cfg.Dests)
	changed := false

	var wg sync.WaitGroup
	wg.Add(lens - 1)

	for i := 1; i < lens; i++ {
		go func(i int) {
			defer wg.Done()
			url := "http://" + lb.cfg.Dests[i].IPAddr.String() + lb.cfg.HealthCheckDest
			res := HealthCheckSingle(url)
			new_status := NewBackendStatus(lb.backendStatus[i], res)
			if (lb.backendStatus[i] == 3) != (new_status == 3) {
				changed = true
			}
			if new_status == 3 {
				lb.cfg.Dests[i].IsAlive = 1
			} else {
				lb.cfg.Dests[i].IsAlive = 0
			}
			lb.backendStatus[i] = new_status
		}(i)
	}
	wg.Wait()
	return changed
}
