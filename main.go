package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/jsimonetti/rtnetlink"
	log "github.com/sirupsen/logrus"
)

const (
	numberOfMetrics      = 2
	defaultMetricsNumber = 60
	defaultBaseline      = 100
	window               = 300
)

var (
	keys = [...]string{
		"receive_bytes",
		"transmit_bytes",
	}
	stats = [window][len(keys)]uint64{}
	sec   = 0
)

type metricsHandler struct{}

// /raw?n=50 returns last 50 raw metrics
// /metrics?n=51&baseline=1000 returns last 51 baseline metrics with baseline 1000 Byte
func (m *metricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/raw":
		s := r.URL.Query().Get("n")
		n, err := strconv.Atoi(s)
		if err != nil {
			n = defaultMetricsNumber
		}
		i, err := w.Write(m.rawMetrics(n))
		if err != nil {
			log.Errorf("Failed to write rawMetrics at %d byte: %v", i, err)
		}
	case "/metrics":
		s := r.URL.Query().Get("baseline")
		k, err := strconv.Atoi(s)
		if err != nil {
			k = defaultBaseline
		}

		s = r.URL.Query().Get("n")
		n, err := strconv.Atoi(s)
		if err != nil {
			n = defaultMetricsNumber
		}
		i, err := w.Write(m.baselineMetrics(k, n))
		if err != nil {
			log.Errorf("Failed to write baselineMetrics at %d byte: %v", i, err)
		}
	}
}

func (m *metricsHandler) baselineMetrics(baseline, n int) []byte {
	if 0 >= n || n >= window {
		log.Errorf("Failed to get rawMetrics valid range for n: 0 < %d < %d", n, window)
		return nil
	}

	cur := sec
	buf := &bytes.Buffer{}
	countAboveBaseline := 0
	countAreaAboveBaseline := uint64(0)

	// get last n values from ring structure
	for j := cur - n; j < cur; j++ {
		// x is always positive, because j can be negative adjust by +window, %window is the ring
		x := (j + window) % window
		stat := stats[x]
		prevX := (j - 1 + window) % window
		prevStat := stats[prevX]

		bytesTotalGradient := uint64(0)
		for i := range keys {
			bytesTotalGradient += stat[i] - prevStat[i]
		}
		if bytesTotalGradient > uint64(baseline) {
			countAboveBaseline++
			countAreaAboveBaseline += bytesTotalGradient - uint64(baseline)
		}
	}

	// % curl http://localhost:8080/metrics\?baseline\=10000\&n\=110
	// above_baseline_count 35
	// above_baseline_area_sum 531161
	fmt.Fprintf(buf, "above_baseline_count %d\n", countAboveBaseline)
	fmt.Fprintf(buf, "above_baseline_area_sum %d\n", countAreaAboveBaseline)

	return buf.Bytes()
}

func (m *metricsHandler) rawMetrics(n int) []byte {
	if 0 >= n || n >= window {
		log.Errorf("Failed to get rawMetrics valid range for n: 0 < %d < %d", n, window)
		return nil
	}

	buf := &bytes.Buffer{}
	cur := sec

	// get last n values from ring structure
	for j := cur - n; j < cur; j++ {
		// x is always positive, because j can be negative adjust by +window, %window is the ring
		x := (j + window) % window
		stat := stats[x]

		// % curl http://localhost:8080/raw\?n\=2
		// 267 receive_bytes 19305901433
		// 267 transmit_bytes 9003338538
		// 268 receive_bytes 19305902078
		// 268 transmit_bytes 9003338768
		for i, k := range keys {
			fmt.Fprintf(buf, "%d %s %v\n", x, k, stat[i])
		}
	}
	return buf.Bytes()
}

func runServer(quit <-chan struct{}) {
	s := &http.Server{
		Addr:           ":8080",
		Handler:        &metricsHandler{},
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	go func() {
		<-quit
		log.Info("stop http server..")
		if err := s.Shutdown(context.Background()); err != nil {
			log.Errorf("Failed to graceful shutdown: %v", err)
		}
		log.Info("stopped http server")
	}()
	if err := s.ListenAndServe(); err != http.ErrServerClosed {
		log.Errorf("ListenAndServe failed: %v", err)
	}
}

func startUpdaterLoop(quit <-chan struct{}, d time.Duration, dev string) {
	filter := newDeviceFilter("", dev)
	ticker := time.NewTicker(d)

	// fill once
	_s, err := getNetDevStats(&filter)
	if err != nil {
		log.Fatalf("Failed to get net dev stats %v", err)
	}
	for j := 0; j < window; j++ {
		for i, k := range keys {
			stats[j][i] = _s[dev][k]
		}
	}

	for _ = range ticker.C {
		select {
		case <-quit:
			log.Info("stopped stats updater loop")
			return
		default:
		}

		_stats, err := getNetDevStats(&filter)
		if err != nil {
			log.Fatalf("Failed to get net dev stats %v", err)
		}
		sec = int(time.Now().Unix() % int64(window))
		for i, k := range keys {
			stats[sec][i] = _stats[dev][k]
		}
		sec = (sec + 1) % window
	}
}

func main() {
	var (
		device string
	)
	flag.StringVar(&device, "device", "wlan0", "device to monitor")
	flag.Parse()

	quit := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	go startUpdaterLoop(quit, 1*time.Second, device)
	go runServer(quit)

	<-sigs
	close(quit)
	time.Sleep(1500 * time.Millisecond)
	log.Infof("done.")
}

// https://github.com/prometheus/node_exporter/blob/ba8c043079b38748e57adf1f80e3d86a4060efc5/collector/device_filter.go
type deviceFilter struct {
	ignorePattern *regexp.Regexp
	acceptPattern *regexp.Regexp
}

func newDeviceFilter(ignoredPattern, acceptPattern string) (f deviceFilter) {
	if ignoredPattern != "" {
		f.ignorePattern = regexp.MustCompile(ignoredPattern)
	}

	if acceptPattern != "" {
		f.acceptPattern = regexp.MustCompile(acceptPattern)
	}

	return
}

// ignores returns whether the device should be ignored
func (f *deviceFilter) ignored(name string) bool {
	return ((f.ignorePattern != nil && f.ignorePattern.MatchString(name)) ||
		(f.acceptPattern != nil && !f.acceptPattern.MatchString(name)))
}

// https://github.com/prometheus/node_exporter/blob/ba8c043079b38748e57adf1f80e3d86a4060efc5/collector/netdev_common.go#L50
type netDevStats map[string]map[string]uint64

// https://github.com/prometheus/node_exporter/blob/ba8c043079b38748e57adf1f80e3d86a4060efc5/collector/netdev_linux.go
func getNetDevStats(filter *deviceFilter) (netDevStats, error) {
	return netlinkStats(filter)
}

func netlinkStats(filter *deviceFilter) (netDevStats, error) {
	conn, err := rtnetlink.Dial(nil)
	if err != nil {
		return nil, err
	}

	defer conn.Close()
	links, err := conn.Link.List()
	if err != nil {
		return nil, err
	}

	return parseNetlinkStats(links, filter), nil
}

func parseNetlinkStats(links []rtnetlink.LinkMessage, filter *deviceFilter) netDevStats {
	metrics := netDevStats{}

	for _, msg := range links {
		name := msg.Attributes.Name
		stats := msg.Attributes.Stats64

		if filter.ignored(name) {
			continue
		}

		// https://github.com/torvalds/linux/blob/master/include/uapi/linux/if_link.h#L42-L246
		metrics[name] = map[string]uint64{
			"receive_packets":  stats.RXPackets,
			"transmit_packets": stats.TXPackets,
			"receive_bytes":    stats.RXBytes,
			"transmit_bytes":   stats.TXBytes,
			"receive_errors":   stats.RXErrors,
			"transmit_errors":  stats.TXErrors,
			"receive_dropped":  stats.RXDropped,
			"transmit_dropped": stats.TXDropped,
			"multicast":        stats.Multicast,
			"collisions":       stats.Collisions,

			// detailed rx_errors
			"receive_length_errors": stats.RXLengthErrors,
			"receive_over_errors":   stats.RXOverErrors,
			"receive_crc_errors":    stats.RXCRCErrors,
			"receive_frame_errors":  stats.RXFrameErrors,
			"receive_fifo_errors":   stats.RXFIFOErrors,
			"receive_missed_errors": stats.RXMissedErrors,

			// detailed tx_errors
			"transmit_aborted_errors":   stats.TXAbortedErrors,
			"transmit_carrier_errors":   stats.TXCarrierErrors,
			"transmit_fifo_errors":      stats.TXFIFOErrors,
			"transmit_heartbeat_errors": stats.TXHeartbeatErrors,
			"transmit_window_errors":    stats.TXWindowErrors,

			// for cslip etc
			"receive_compressed":  stats.RXCompressed,
			"transmit_compressed": stats.TXCompressed,
			"receive_nohandler":   stats.RXNoHandler,
		}
	}

	return metrics
}

// /proc/net/dev read stats by file
// func procNetDevStats(filter *deviceFilter, logger log.Logger) (netDevStats, error) {
// 	metrics := netDevStats{}

// 	fs, err := procfs.NewFS(*procPath)
// 	if err != nil {
// 		return metrics, fmt.Errorf("failed to open procfs: %w", err)
// 	}

// 	netDev, err := fs.NetDev()
// 	if err != nil {
// 		return metrics, fmt.Errorf("failed to parse /proc/net/dev: %w", err)
// 	}

// 	for _, stats := range netDev {
// 		name := stats.Name

// 		if filter.ignored(name) {
// 			level.Debug(logger).Log("msg", "Ignoring device", "device", name)
// 			continue
// 		}

// 		metrics[name] = map[string]uint64{
// 			"receive_bytes":       stats.RxBytes,
// 			"receive_packets":     stats.RxPackets,
// 			"receive_errors":      stats.RxErrors,
// 			"receive_dropped":     stats.RxDropped,
// 			"receive_fifo":        stats.RxFIFO,
// 			"receive_frame":       stats.RxFrame,
// 			"receive_compressed":  stats.RxCompressed,
// 			"receive_multicast":   stats.RxMulticast,
// 			"transmit_bytes":      stats.TxBytes,
// 			"transmit_packets":    stats.TxPackets,
// 			"transmit_errors":     stats.TxErrors,
// 			"transmit_dropped":    stats.TxDropped,
// 			"transmit_fifo":       stats.TxFIFO,
// 			"transmit_colls":      stats.TxCollisions,
// 			"transmit_carrier":    stats.TxCarrier,
// 			"transmit_compressed": stats.TxCompressed,
// 		}
// 	}

// 	return metrics, nil
// }
