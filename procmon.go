package main

import (
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v3/process"

	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
	"gonum.org/v1/plot/vg/draw"
	"gonum.org/v1/plot/vg/vgimg"
)

type sample struct {
	TS         time.Time     `json:"ts"`
	Elapsed    time.Duration `json:"elapsed"`
	CPUPercent float64       `json:"cpu_percent"`
	RSSBytes   uint64        `json:"rss_bytes"`
	ProcCount  int           `json:"proc_count"`
}

func main() {
	var (
		interval  = flag.Duration("interval", 500*time.Millisecond, "sampling interval (e.g. 200ms, 1s)")
		outPath   = flag.String("out", "", "optional CSV output file (default: ./procmon-YYYYMMDD-HHMMSS.csv)")
		tree      = flag.Bool("tree", true, "aggregate usage across ALL descendants (process tree)")
		quiet     = flag.Bool("quiet", false, "suppress log noise")
		streamIO  = flag.Bool("stream", true, "stream child stdout/stderr to this process")
		duration  = flag.Duration("duration", 0, "stop after this duration (e.g. 10s, 2m). 0 = until process exits")
		targetPID = flag.Int("pid", 0, "monitor an existing PID instead of spawning a command")
		httpAddr  = flag.String("http", "", "serve live UI on this address (e.g. ':8080'); empty disables web UI")
		plotPath  = flag.String("plot", "", "write a PNG graph to this path at the end (e.g. stats.png)")
	)

	flag.Usage = func() {
		me := filepath.Base(os.Args[0])
		fmt.Fprintf(os.Stderr, "Usage:\n  %s [flags] -- <command> [args...]\n  %s [flags] -pid <PID>\n\nFlags:\n", me, me)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Examples:
  # Live UI only
  %[1]s --interval=200ms --tree -http :8080 -- gzip -9 bigfile

  # PNG only
  %[1]s -plot stats.png -- sleep 3

  # Both live UI and PNG
  %[1]s -http :8080 -plot stats.png -- sleep 3

  # Attach to PID for 30s
  %[1]s -pid 12345 -duration=30s -http :8080 -plot pid12345.png
`, me)
	}

	// Split args at "--"
	dashdash := -1
	for i, a := range os.Args {
		if a == "--" {
			dashdash = i
			break
		}
	}
	var cmdArgs []string
	if dashdash >= 0 {
		_ = flag.CommandLine.Parse(os.Args[1:dashdash])
		cmdArgs = os.Args[dashdash+1:]
	} else {
		flag.Parse()
		cmdArgs = flag.Args()
	}

	if *targetPID > 0 && len(cmdArgs) > 0 {
		log.Fatalf("cannot use -pid with a command; choose one mode")
	}
	if *targetPID == 0 && len(cmdArgs) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	if *quiet {
		logger.SetOutput(newDevNull())
	}

	// CSV setup (optional)
	var wcsv *csv.Writer
	if *outPath != "" {
		if dir := filepath.Dir(*outPath); dir != "." {
			_ = os.MkdirAll(dir, 0o755)
		}
		fh, err := os.Create(*outPath)
		if err != nil {
			log.Fatalf("create output: %v", err)
		}
		defer fh.Close()
		wcsv = csv.NewWriter(fh)
		writeRow(wcsv, "timestamp", "elapsed_sec", "cpu_percent_total", "rss_bytes_total", "process_count")
		defer wcsv.Flush()
	}
	// Ensure plot dir exists if needed
	if *plotPath != "" {
		if dir := filepath.Dir(*plotPath); dir != "." {
			_ = os.MkdirAll(dir, 0o755)
		}
	}

	// Start or attach
	var (
		start   = time.Now()
		rootP   *process.Process
		cmd     *exec.Cmd
		monMode string
		err     error
	)
	if *targetPID > 0 {
		monMode = "pid"
		rootP, err = process.NewProcess(int32(*targetPID))
		if err != nil {
			log.Fatalf("attach PID %d: %v", *targetPID, err)
		}
		logger.Printf("attaching to PID %d", *targetPID)
	} else {
		monMode = "spawn"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cmd = exec.CommandContext(ctx, cmdArgs[0], cmdArgs[1:]...)
		if *streamIO {
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
		}
		logger.Printf("starting: %s", strings.Join(cmdArgs, " "))
		if err := cmd.Start(); err != nil {
			log.Fatalf("start command: %v", err)
		}
		rootP, err = process.NewProcess(int32(cmd.Process.Pid))
		if err != nil {
			log.Fatalf("get process: %v", err)
		}
	}

	// Waiter for subcommand (spawn mode)
	var waitCh chan struct{}
	if monMode == "spawn" {
		waitCh = make(chan struct{}, 1)
		go func() {
			_ = cmd.Wait()
			waitCh <- struct{}{}
		}()
	}

	// Sampler & hub
	primeProcesses(rootP, *tree)
	hub := newHub()
	go hub.run()

	// HTTP (optional)
	var stopHTTP chan struct{}
	if *httpAddr != "" {
		stopHTTP = make(chan struct{})
		go func() {
			if err := serveHTTP(*httpAddr, hub, stopHTTP); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Printf("http error: %v", err)
			}
		}()
		logger.Printf("web UI: http://localhost%s", *httpAddr)
	}

	// Optional timeout
	var timeout <-chan time.Time
	if *duration > 0 {
		t := time.NewTimer(*duration)
		defer t.Stop()
		timeout = t.C
	}

	// First sample
	s := takeSample(rootP, start, *tree)
	hub.addSample(s)
	if wcsv != nil {
		writeCSVSample(wcsv, s)
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ticker.C:
			s := takeSample(rootP, start, *tree)
			hub.addSample(s)
			if wcsv != nil {
				writeCSVSample(wcsv, s)
			}
			if monMode == "pid" && !isRunning(rootP) {
				break loop
			}

		case <-waitCh: // subcommand finished
			// Optional final sample post-exit
			s := takeSample(rootP, start, *tree)
			hub.addSample(s)
			if wcsv != nil {
				writeCSVSample(wcsv, s)
			}
			break loop

		case <-timeout:
			break loop
		}
	}

	// Final sample (defensive)
	s = takeSample(rootP, start, *tree)
	hub.addSample(s)
	if wcsv != nil {
		writeCSVSample(wcsv, s)
	}

	// Write PNG if requested
	if *plotPath != "" {
		if err := renderPlots(hub.snapshot(), *plotPath); err != nil {
			log.Printf("plot error: %v", err)
		} else {
			log.Printf("wrote plot to %s", *plotPath)
		}
	}

	log.Printf("done%s", ternary(*outPath != "", " (CSV written to "+*outPath+")", ""))

	// Shutdown HTTP if running
	if stopHTTP != nil {
		close(stopHTTP)
		time.Sleep(120 * time.Millisecond) // small grace for clients
	}

	// Signal clients
	hub.broadcastEvent("done", map[string]any{"message": "process finished"})
	time.Sleep(100 * time.Millisecond)
}

// ===== process sampling =====

func primeProcesses(root *process.Process, includeTree bool) {
	plist := []*process.Process{root}
	if includeTree {
		if kids, err := root.Children(); err == nil {
			plist = append(plist, kids...)
		}
	}
	for _, p := range plist {
		_, _ = p.Percent(0) // delta baseline
	}
}

func listProcTree(root *process.Process) []*process.Process {
	seen := map[int32]struct{}{}
	out := make([]*process.Process, 0, 16)
	q := []*process.Process{root}
	for len(q) > 0 {
		p := q[0]
		q = q[1:]
		if p == nil {
			continue
		}
		pid := p.Pid
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		out = append(out, p)
		if kids, err := p.Children(); err == nil {
			for _, k := range kids {
				if k != nil {
					if _, ok := seen[k.Pid]; !ok {
						q = append(q, k)
					}
				}
			}
		}
	}
	return out
}

func takeSample(root *process.Process, start time.Time, includeTree bool) sample {
	ts := time.Now()
	var cpu float64
	var rss uint64
	var n int

	plist := []*process.Process{root}
	if includeTree {
		plist = listProcTree(root)
	}

	for _, p := range plist {
		if p == nil {
			continue
		}
		if v, err := p.Percent(0); err == nil {
			cpu += v
		}
		if mi, err := p.MemoryInfo(); err == nil && mi != nil {
			rss += mi.RSS
		}
		n++
	}

	return sample{
		TS:         ts,
		Elapsed:    ts.Sub(start),
		CPUPercent: cpu, // may exceed 100 on multi-core
		RSSBytes:   rss,
		ProcCount:  n,
	}
}

func isRunning(p *process.Process) bool {
	ok, err := p.IsRunning()
	return err == nil && ok
}

func writeCSVSample(w *csv.Writer, s sample) {
	writeRow(w,
		s.TS.Format(time.RFC3339Nano),
		fmt.Sprintf("%.3f", s.Elapsed.Seconds()),
		fmt.Sprintf("%.3f", s.CPUPercent),
		strconv.FormatUint(s.RSSBytes, 10),
		strconv.Itoa(s.ProcCount),
	)
	w.Flush()
}

func writeRow(w *csv.Writer, fields ...string) { _ = w.Write(fields) }

// ===== HTTP server (SSE + static) =====

//go:embed web/*
var webFS embed.FS

func serveHTTP(addr string, hub *hub, stop <-chan struct{}) error {
	mux := http.NewServeMux()

	// Serve embedded /web at /
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	mux.Handle("/", http.FileServer(http.FS(webRoot)))

	// Initial samples (JSON)
	mux.HandleFunc("/api/samples", func(w http.ResponseWriter, r *http.Request) {
		hub.mu.RLock()
		defer hub.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hub.samples)
	})

	// SSE stream
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		ch := make(chan sseMsg, 16)
		hub.register <- ch
		defer func() { hub.unregister <- ch }()

		// hello
		fmt.Fprintf(w, "event: hello\ndata: %s\n\n", `{"ok":true}`)
		flusher.Flush()

		notify := r.Context().Done()
		for {
			select {
			case msg := <-ch:
				fmt.Fprintf(w, "event: %s\n", msg.Event)
				b, _ := json.Marshal(msg.Data)
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			case <-notify:
				return
			}
		}
	})

	srv := &http.Server{Addr: addr, Handler: mux}

	// graceful shutdown when we're done sampling
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	return srv.ListenAndServe()
}

// ===== SSE hub =====

type sseMsg struct {
	Event string
	Data  any
}

type hub struct {
	mu         sync.RWMutex
	samples    []sample
	register   chan chan sseMsg
	unregister chan chan sseMsg
	clients    map[chan sseMsg]struct{}
}

func newHub() *hub {
	return &hub{
		samples:    make([]sample, 0, 4096),
		register:   make(chan chan sseMsg),
		unregister: make(chan chan sseMsg),
		clients:    make(map[chan sseMsg]struct{}),
	}
}

func (h *hub) run() {
	for {
		select {
		case ch := <-h.register:
			h.clients[ch] = struct{}{}
		case ch := <-h.unregister:
			delete(h.clients, ch)
			close(ch)
		}
	}
}

func (h *hub) addSample(s sample) {
	h.mu.Lock()
	h.samples = append(h.samples, s)
	h.mu.Unlock()
	h.broadcastEvent("sample", s)
}

func (h *hub) broadcastEvent(name string, data any) {
	msg := sseMsg{Event: name, Data: data}
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
			// slow client—drop
		}
	}
}

func (h *hub) snapshot() []sample {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]sample, len(h.samples))
	copy(out, h.samples)
	return out
}

// ===== PNG plotting (single merged chart) =====
// CPU% (total) and CPU-cores line (NumCPU*100) on left Y; RAM MiB on right Y.
// ===== PNG plotting (two stacked charts: CPU w/cores + RAM) =====
// ===== PNG plotting (styled: CPU w/cores + RAM, stacked) =====
func renderPlots(samples []sample, outPath string) error {
	if len(samples) == 0 {
		return nil
	}

	cpuPts := make(plotter.XYs, 0, len(samples))
	corePts := make(plotter.XYs, 0, len(samples))
	memPts := make(plotter.XYs, 0, len(samples))

	cores := float64(runtime.NumCPU() * 100)
	for _, s := range samples {
		x := s.Elapsed.Seconds()
		cpuPts = append(cpuPts, plotter.XY{X: x, Y: s.CPUPercent})
		corePts = append(corePts, plotter.XY{X: x, Y: cores})
		mb := float64(s.RSSBytes) / (1024 * 1024)
		memPts = append(memPts, plotter.XY{X: x, Y: mb})
	}

	// Common palette
	cCPU := color.RGBA{R: 33, G: 150, B: 243, A: 255}   // blue
	cCore := color.RGBA{R: 120, G: 144, B: 156, A: 255} // blue-grey (dashed)
	cRAM := color.RGBA{R: 76, G: 175, B: 80, A: 255}    // green

	// ---- CPU plot (top) ----
	cpuPlot := plot.New()
	cpuPlot.Title.Text = "CPU usage"
	cpuPlot.BackgroundColor = color.White
	cpuPlot.X.Label.Text = "Elapsed (s)"
	cpuPlot.Y.Label.Text = "CPU (%)"
	cpuPlot.Add(plotter.NewGrid())

	cpuLine, err := plotter.NewLine(cpuPts)
	if err != nil {
		return err
	}
	cpuLine.Color = cCPU
	cpuLine.Width = vg.Points(1.8)

	coreLine, err := plotter.NewLine(corePts)
	if err != nil {
		return err
	}
	coreLine.Color = cCore
	coreLine.Width = vg.Points(1.2)
	coreLine.LineStyle.Dashes = []vg.Length{vg.Points(6), vg.Points(6)}

	cpuPlot.Add(cpuLine, coreLine)
	cpuPlot.Legend.Top = true
	cpuPlot.Legend.TextStyle.Font.Size = vg.Points(10)
	cpuPlot.Legend.Add("CPU % (total)", cpuLine)
	cpuPlot.Legend.Add(fmt.Sprintf("CPU cores ×100%% (%d)", runtime.NumCPU()), coreLine)

	// ---- RAM plot (bottom) ----
	memPlot := plot.New()
	memPlot.Title.Text = "Memory (RSS)"
	memPlot.BackgroundColor = color.White
	memPlot.X.Label.Text = "Elapsed (s)"
	memPlot.Y.Label.Text = "RAM (MiB)"
	memPlot.Add(plotter.NewGrid())

	memLine, err := plotter.NewLine(memPts)
	if err != nil {
		return err
	}
	memLine.Color = cRAM
	memLine.Width = vg.Points(1.8)

	memPlot.Add(memLine)
	memPlot.Legend.Top = true
	memPlot.Legend.TextStyle.Font.Size = vg.Points(10)
	memPlot.Legend.Add("RAM (MiB)", memLine)

	// Canvas ~1200x700 px, two rows
	img := vgimg.New(vg.Points(1200), vg.Points(700))
	dc := draw.New(img)
	tiles := draw.Tiles{
		Rows:      2,
		Cols:      1,
		PadX:      vg.Millimeter * 3,
		PadY:      vg.Millimeter * 3,
		PadTop:    vg.Millimeter * 4,
		PadBottom: vg.Millimeter * 3,
		PadLeft:   vg.Millimeter * 6,
		PadRight:  vg.Millimeter * 6,
	}

	canvases := plot.Align([][]*plot.Plot{
		{cpuPlot},
		{memPlot},
	}, tiles, dc)

	cpuPlot.Draw(canvases[0][0])
	memPlot.Draw(canvases[1][0])

	// Write PNG
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = vgimg.PngCanvas{Canvas: img}.WriteTo(f)
	return err
}

// ===== helpers =====

type devNull struct{ f *os.File }

func newDevNull() *devNull {
	f, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	return &devNull{f: f}
}
func (d *devNull) Write(p []byte) (int, error) {
	if d.f == nil {
		return len(p), nil
	}
	return d.f.Write(p)
}
func ternary[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
