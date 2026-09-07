package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type sample struct {
	at  time.Time
	rtt float64
	err error
}

// Match individual replies only, never an aggregate or a TTL field.
var replyTime = regexp.MustCompile(`(?i)(?:time|время)\s*([=<])\s*([0-9]+(?:[.,][0-9]+)?)\s*(?:ms|мс)`)
var numericField = regexp.MustCompile(`([=<])\s*([0-9]+(?:[.,][0-9]+)?)`)
var ttlField = regexp.MustCompile(`(?i)\bTTL=`)

func parseRTT(output string) (float64, error) {
	m := replyTime.FindStringSubmatch(output)
	// Windows uses the console code page for localized labels. IPv4 reply
	// lines end with TTL; the preceding numeric field is the reply time.
	if m == nil {
		for _, line := range strings.Split(output, "\n") {
			// Find byte offsets in the original output, which may be OEM-encoded.
			if loc := ttlField.FindStringIndex(line); loc != nil {
				fields := numericField.FindAllStringSubmatch(line[:loc[0]], -1)
				if len(fields) >= 2 {
					m = fields[len(fields)-1]
					break
				}
			}
		}
	}
	if m == nil {
		return 0, errors.New("ответ не содержит времени RTT")
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(m[2], ",", "."), 64)
	// Windows reports time<1ms. Use the midpoint of that interval.
	if m[1] == "<" {
		v /= 2
	}
	return v, err
}

func pingArgs(platform, address string, timeout time.Duration) []string {
	ms := strconv.FormatInt(max(1, timeout.Milliseconds()), 10)
	switch platform {
	case "windows":
		return []string{"-n", "1", "-w", ms, address}
	case "darwin":
		return []string{"-n", "-c", "1", "-W", ms, address}
	default:
		return []string{"-n", "-c", "1", "-W", strconv.Itoa(max(1, int(math.Ceil(timeout.Seconds())))), address}
	}
}

func probe(ctx context.Context, binary, address string, timeout time.Duration) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, pingArgs(runtime.GOOS, address, timeout)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, fmt.Errorf("нет успешного ответа (%v)", err)
	}
	return parseRTT(string(out))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	x := p / 100 * float64(len(sorted)-1)
	i, j := int(math.Floor(x)), int(math.Ceil(x))
	return sorted[i] + (sorted[j]-sorted[i])*(x-float64(i))
}

func report(samples []sample, start, end time.Time, slow time.Duration) {
	fmt.Printf("\n──────── Отчёт ────────\nПериод: %s — %s\nДлительность: %s\n", start.Format(time.RFC3339), end.Format(time.RFC3339), end.Sub(start).Round(time.Millisecond))
	if len(samples) == 0 {
		fmt.Println("Нет завершённых измерений.")
		return
	}
	values := []float64{}
	var sum, jitter float64
	jitterN := 0
	for i, s := range samples {
		if s.err != nil {
			continue
		}
		values = append(values, s.rtt)
		sum += s.rtt
		if i > 0 && samples[i-1].err == nil {
			jitter += math.Abs(s.rtt - samples[i-1].rtt)
			jitterN++
		}
	}
	lost := len(samples) - len(values)
	fmt.Printf("Завершено попыток: %d; ответов: %d; без ответа/ошибок: %d (%.2f%%)\n", len(samples), len(values), lost, 100*float64(lost)/float64(len(samples)))
	if len(values) > 0 {
		sort.Float64s(values)
		mean := sum / float64(len(values))
		var variance float64
		for _, v := range values {
			variance += (v - mean) * (v - mean)
		}
		fmt.Printf("RTT, мс: min %.3f | avg %.3f | max %.3f | stddev %.3f\n", values[0], mean, values[len(values)-1], math.Sqrt(variance/float64(len(values))))
		fmt.Printf("Перцентили, мс: p50 %.3f | p90 %.3f | p95 %.3f | p99 %.3f\n", percentile(values, 50), percentile(values, 90), percentile(values, 95), percentile(values, 99))
		if jitterN > 0 {
			fmt.Printf("Джиттер (средняя |ΔRTT| соседних успешных попыток): %.3f мс\n", jitter/float64(jitterN))
		}
	}
	fmt.Printf("\nПериоды ухудшения (RTT >= %s или ошибка):\n", slow)
	bad := func(s sample) bool { return s.err != nil || s.rtt >= float64(slow)/float64(time.Millisecond) }
	found := false
	for i := 0; i < len(samples); {
		if !bad(samples[i]) {
			i++
			continue
		}
		found = true
		j, failures, peak := i, 0, 0.0
		for j < len(samples) && bad(samples[j]) {
			if samples[j].err != nil {
				failures++
			} else {
				peak = math.Max(peak, samples[j].rtt)
			}
			j++
		}
		until, suffix := end, "восстановление не зафиксировано"
		if j < len(samples) {
			until, suffix = samples[j].at, "до первого нормального измерения"
		}
		fmt.Printf("  %s — %s (~%s): %d попыток, %d ошибок", samples[i].at.Format("2006-01-02 15:04:05.000Z07:00"), until.Format("15:04:05.000Z07:00"), until.Sub(samples[i].at).Round(time.Millisecond), j-i, failures)
		if j-i > failures {
			fmt.Printf(", пик %.3f мс", peak)
		}
		fmt.Printf("; %s\n", suffix)
		i = j
	}
	if !found {
		fmt.Println("  Не обнаружены.")
	}
	fmt.Println("Границы периодов приблизительные: по времени начала проб. Ошибки включают таймауты и локальные сбои.")
}

func run() error {
	interval := flag.Duration("interval", time.Second, "интервал между началами проб")
	timeout := flag.Duration("timeout", 2*time.Second, "таймаут одной пробы")
	slow := flag.Duration("slow", 100*time.Millisecond, "порог ухудшения RTT")
	count := flag.Int("count", 0, "число проб (0 — до Ctrl+C)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Использование: %s [флаги] адрес\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return errors.New("укажите один IP-адрес или hostname")
	}
	if *interval <= 0 || *timeout <= 0 || *slow <= 0 || *count < 0 {
		return errors.New("длительности должны быть > 0, count >= 0")
	}
	binary, err := exec.LookPath("ping")
	if err != nil {
		return errors.New("системная команда ping не найдена; установите её и добавьте в PATH")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	resolveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	addresses, err := net.DefaultResolver.LookupIPAddr(resolveCtx, flag.Arg(0))
	cancel()
	if err != nil {
		return fmt.Errorf("разрешение адреса: %w", err)
	}
	// IPv4 works with the same system command on all supported platforms.
	address := ""
	for _, a := range addresses {
		if a.IP.To4() != nil {
			address = a.IP.String()
			break
		}
	}
	if address == "" {
		return errors.New("IPv4-адрес не найден; эта версия поддерживает IPv4")
	}
	fmt.Printf("PING %s (%s), интервал %s, таймаут %s, порог %s\nCtrl+C — закончить и вывести отчёт (Windows/Linux/macOS).\n", flag.Arg(0), address, *interval, *timeout, *slow)
	start := time.Now()
	var samples []sample
	for ctx.Err() == nil && (*count == 0 || len(samples) < *count) {
		at := time.Now()
		rtt, err := probe(ctx, binary, address, *timeout)
		if ctx.Err() != nil {
			break
		} // Interrupted probes are not counted as loss.
		samples = append(samples, sample{at: at, rtt: rtt, err: err})
		if err != nil {
			fmt.Printf("%s #%d ERROR %v\n", at.Format("15:04:05.000"), len(samples), err)
		} else {
			label := ""
			if rtt >= float64(*slow)/float64(time.Millisecond) {
				label = " SLOW"
			}
			fmt.Printf("%s #%d RTT %.3f ms%s\n", at.Format("15:04:05.000"), len(samples), rtt, label)
		}
		if *count > 0 && len(samples) >= *count {
			break
		}
		timer := time.NewTimer(max(0, *interval-time.Since(at)))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	report(samples, start, time.Now(), *slow)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Ошибка:", err)
		os.Exit(1)
	}
}
