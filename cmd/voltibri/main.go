// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

// Command voltibri is the smart self-consumption optimizer.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/voltibri/voltibri/internal/api"
	"github.com/voltibri/voltibri/internal/calendar"
	"github.com/voltibri/voltibri/internal/config"
	_ "github.com/voltibri/voltibri/internal/drivers/all"
	"github.com/voltibri/voltibri/internal/forecast"
	"github.com/voltibri/voltibri/internal/importer"
	"github.com/voltibri/voltibri/internal/planner"
	"github.com/voltibri/voltibri/internal/series"
	"github.com/voltibri/voltibri/internal/server"
	"github.com/voltibri/voltibri/internal/sim"
	"github.com/voltibri/voltibri/internal/store"
	"github.com/voltibri/voltibri/sdk"
)

var version = "dev"

const usage = `voltibri – smart self-consumption optimizer

Usage:
  voltibri serve     [-data DIR] [-config FILE]  run the control loop and web UI
  voltibri config    import|export|versions|rollback|validate ...
  voltibri history   info|import|import-ha ...  stored measurement history
  voltibri simulate  [-input data.csv]           compare charging strategies on history
  voltibri synth     -out data.csv               generate synthetic demo data
  voltibri import-ha -in history.csv -out data.csv -pv ... -grid ...
  voltibri drivers                               list available drivers
  voltibri version

Run "voltibri <command> -h" for the options of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "config":
		err = configCmd(os.Args[2:])
	case "history":
		err = historyCmd(os.Args[2:])
	case "simulate":
		err = simulate(os.Args[2:], os.Stdout)
	case "synth":
		err = synth(os.Args[2:])
	case "import-ha":
		err = importHA(os.Args[2:])
	case "drivers":
		fmt.Println(strings.Join(sdk.Types(), "\n"))
	case "version":
		fmt.Println("voltibri", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const historyUsage = `Usage:
  voltibri history info      [-data DIR]                         show stored history
  voltibri history import    [-data DIR] [-overwrite] FILE.csv   import voltibri CSV (time,pv_w,load_w,...)
  voltibri history import-ha [-data DIR] [-overwrite] -in EXPORT.csv -pv ENTITIES (-load ENTITY | -grid ENTITY [-battery ENTITY]) [-temp ENTITY]
                                                                 import a Home Assistant history export

Existing slots (voltibri's own measurements) are kept unless -overwrite is given.
Restart "voltibri serve" afterwards so the forecaster learns from the imported data.
`

func historyCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(historyUsage)
		return errors.New("missing subcommand")
	}
	fs := flag.NewFlagSet("history "+args[0], flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "data directory, env VOLTIBRI_DATA")
	overwrite := fs.Bool("overwrite", false, "replace existing slots")
	tz := fs.String("tz", "Europe/Berlin", "time zone")
	in := fs.String("in", "", "Home Assistant history export (CSV)")
	pv := fs.String("pv", "", "PV power entities, comma separated")
	load := fs.String("load", "", "house load entity")
	grid := fs.String("grid", "", "grid power entity (+import), used if -load is empty")
	bat := fs.String("battery", "", "battery power entity (+discharge), used if -load is empty")
	temp := fs.String("temp", "", "outdoor temperature entity")
	scale := fs.Float64("scale", 1, "power scale (1 = W, 1000 = kW)")
	fs.Parse(args[1:])
	ctx := context.Background()
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return err
	}
	st, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	var recs series.Series
	switch args[0] {
	case "info":
		hi, err := st.HistoryInfo(ctx)
		if err != nil {
			return err
		}
		if hi.Slots == 0 {
			fmt.Println("Noch kein Verlauf gespeichert.")
			return nil
		}
		fmt.Printf("%d Viertelstunden (%.1f Tage) von %s bis %s\n", hi.Slots, float64(hi.Slots)/96,
			hi.First.In(loc).Format("02.01.2006 15:04"), hi.Last.In(loc).Format("02.01.2006 15:04"))
		return nil
	case "import":
		if fs.NArg() != 1 {
			return errors.New("usage: voltibri history import FILE.csv")
		}
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return err
		}
		recs, err = series.ReadCSV(bufio.NewReader(f), loc)
		f.Close()
		if err != nil {
			return err
		}
	case "import-ha":
		if *in == "" || *pv == "" {
			return errors.New("-in and -pv are required")
		}
		f, err := os.Open(*in)
		if err != nil {
			return err
		}
		recs, err = importer.ConvertHA(bufio.NewReader(f), importer.Mapping{
			PV: strings.Split(*pv, ","), Load: *load, Grid: *grid, Battery: *bat, Temp: *temp, Scale: *scale,
		}, loc)
		f.Close()
		if err != nil {
			return err
		}
	default:
		fmt.Print(historyUsage)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
	if len(recs) == 0 {
		return errors.New("no usable data found")
	}
	n, err := st.ImportHistory(ctx, recs, *overwrite)
	if err != nil {
		return err
	}
	fmt.Printf("%d von %d Viertelstunden importiert (%s bis %s).\n", n, len(recs),
		recs[0].Time.In(loc).Format("02.01.2006 15:04"), recs[len(recs)-1].Time.In(loc).Format("02.01.2006 15:04"))
	if n < len(recs) {
		fmt.Println("Bereits vorhandene Viertelstunden wurden behalten (-overwrite ersetzt sie).")
	}
	fmt.Println("Bitte \"voltibri serve\" neu starten, damit die Prognose die Daten lernt.")
	return nil
}

func defaultDataDir() string {
	if d := os.Getenv("VOLTIBRI_DATA"); d != "" {
		return d
	}
	return "data"
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "data directory (database and secret key), env VOLTIBRI_DATA")
	yamlPath := fs.String("config", "", "YAML file that manages the configuration (read-only mode, reloaded on change)")
	listen := fs.String("listen", "", "listen address (default: site.listen from the configuration or 127.0.0.1:7070)")
	fs.Parse(args)

	st, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := *listen
	if addr == "" {
		addr = "127.0.0.1:7070"
		if *yamlPath != "" {
			if cfg, err := config.Load(*yamlPath); err == nil && cfg.Site.Listen != "" {
				addr = cfg.Site.Listen
			}
		} else if cfg, _, err := st.Current(ctx); err == nil && cfg.Site.Listen != "" {
			addr = cfg.Site.Listen
		}
	}

	rt := server.New(st, *yamlPath)
	srv := &http.Server{Addr: addr, Handler: api.Handler(rt, st), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("web UI", "url", "http://"+addr, "data", *dataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server", "err", err)
			stop()
		}
	}()
	err = rt.Run(ctx)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	return err
}

const configUsage = `Usage:
  voltibri config import   [-data DIR] [-m comment] FILE.yaml   store a YAML file as new version
  voltibri config export   [-data DIR] [-secrets]               print the current configuration as YAML
  voltibri config versions [-data DIR]                          list configuration versions
  voltibri config rollback [-data DIR] ID                       restore a version (as new version)
  voltibri config validate FILE.yaml                            check a YAML file
`

func configCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(configUsage)
		return errors.New("missing subcommand")
	}
	fs := flag.NewFlagSet("config "+args[0], flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "data directory, env VOLTIBRI_DATA")
	comment := fs.String("m", "", "comment for the new version")
	secrets := fs.Bool("secrets", false, "include secrets in plain text (careful!)")
	fs.Parse(args[1:])
	ctx := context.Background()

	if args[0] == "validate" {
		if fs.NArg() != 1 {
			return errors.New("usage: voltibri config validate FILE.yaml")
		}
		if _, err := config.Load(fs.Arg(0)); err != nil {
			return err
		}
		fmt.Println("Konfiguration ist gültig.")
		return nil
	}

	st, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	switch args[0] {
	case "import":
		if fs.NArg() != 1 {
			return errors.New("usage: voltibri config import FILE.yaml")
		}
		cfg, err := config.Load(fs.Arg(0))
		if err != nil {
			return err
		}
		c := *comment
		if c == "" {
			c = "Import aus " + fs.Arg(0)
		}
		id, err := st.Save(ctx, cfg, "cli", c)
		if err != nil {
			return err
		}
		fmt.Printf("Version %d gespeichert.\n", id)
	case "export":
		cfg, _, err := st.Current(ctx)
		if err != nil {
			return err
		}
		if !*secrets {
			cfg = store.Redact(cfg)
		}
		b, err := cfg.YAML()
		if err != nil {
			return err
		}
		os.Stdout.Write(b)
	case "versions":
		vs, err := st.Versions(ctx, 50)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "Version\tZeitpunkt\tVon\tKommentar")
		for _, v := range vs {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", v.ID, v.CreatedAt.Format("02.01.2006 15:04"), v.Author, v.Comment)
		}
		tw.Flush()
	case "rollback":
		if fs.NArg() != 1 {
			return errors.New("usage: voltibri config rollback ID")
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil {
			return err
		}
		nid, err := st.Rollback(ctx, id, "cli")
		if err != nil {
			return err
		}
		fmt.Printf("Version %d wiederhergestellt als Version %d.\n", id, nid)
	default:
		fmt.Print(configUsage)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
	return nil
}

func simulate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	input := fs.String("input", "", "CSV with history (time,pv_w,load_w[,spot_ct,temp_c]); empty = synthetic year")
	start := fs.String("start", "2026-01-01", "start date for synthetic data")
	days := fs.Int("days", 365, "days of synthetic data")
	tz := fs.String("tz", "Europe/Berlin", "time zone")
	capWh := fs.Float64("capacity", 13800, "usable battery capacity in Wh")
	maxW := fs.Float64("max-power", 6000, "max battery charge/discharge power in W")
	feedIn := fs.Float64("feedin", 8.0, "fixed feed-in tariff ct/kWh")
	importCt := fs.Float64("import", 30.0, "import price ct/kWh")
	pvErr := fs.Float64("pv-error", 0.15, "daily PV forecast error (sigma, 0 = perfect forecast)")
	buffer := fs.Float64("buffer", 0.25, "morning buffer SoC")
	months := fs.Bool("months", true, "print monthly breakdown")
	fillNeg := fs.Bool("fill-negative", false, "charge without limit while the spot price is negative")
	fs.Parse(args)

	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return err
	}
	cal, _ := calendar.New()
	var data series.Series
	if *input != "" {
		f, err := os.Open(*input)
		if err != nil {
			return err
		}
		data, err = series.ReadCSV(bufio.NewReader(f), loc)
		f.Close()
		if err != nil {
			return err
		}
	} else {
		st, err := time.ParseInLocation("2006-01-02", *start, loc)
		if err != nil {
			return err
		}
		data = sim.Synthesize(st, *days, sim.DefaultSynth(), cal)
		fmt.Fprintf(out, "Synthetische Daten: %d Tage ab %s (10 kWp, Wärmepumpe)\n", *days, *start)
	}
	if len(data) == 0 {
		return errors.New("no data")
	}
	pc := planner.DefaultConfig()
	pc.MorningBufferSoC = *buffer
	pc.FillOnNegativePrice = *fillNeg
	p := sim.Params{
		Battery:  planner.Battery{CapacityWh: *capWh, SoC: 0.5, MaxChargeW: *maxW, MaxDischargeW: *maxW, ChargeEff: 0.95, DischargeEff: 0.95, MinSoC: 0.05, MaxSoC: 1},
		FeedInCt: *feedIn, ImportCt: *importCt, PVForecastError: *pvErr, Seed: 42,
		Planner: pc, Forecast: forecast.DefaultConfig(), Calendar: cal, Location: loc,
	}

	planned := sim.NewPlanned(data, p)
	results := []sim.Result{sim.Run(data, sim.Immediate{}, p), sim.Run(data, planned, p)}
	fmt.Fprintf(out, "Zeitraum: %s bis %s, Batterie %.1f kWh\n\n", data[0].Time.Format("02.01.2006"), data[len(data)-1].Time.Format("02.01.2006"), *capWh/1000)
	printResults(out, results)

	if *months {
		fmt.Fprintln(out, "\nNach Monat (sofort → voltibri):")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(tw, "Monat\tStunden ≥90 %\tLadestand 7 Uhr\tLaden bei neg. Preis kWh\tNetzbezug kWh\t")
		for _, m := range splitMonths(data) {
			a := sim.Run(m, sim.Immediate{}, p)
			pl := sim.NewPlanned(data, p)
			b := runSlice(data, m, pl, p)
			fmt.Fprintf(tw, "%s\t%.0f → %.0f\t%.0f → %.0f %%\t%.0f → %.0f\t%.0f → %.0f\t\n", m[0].Time.Format("01/2006"),
				a.HoursAbove90, b.HoursAbove90, 100*a.MorningSoCMean, 100*b.MorningSoCMean, a.ChargedNegKWh, b.ChargedNegKWh, a.ImportKWh, b.ImportKWh)
		}
		tw.Flush()
		fmt.Fprintln(out, "\nHinweis: Monate werden einzeln mit Start-Ladestand 50 % simuliert; die Summen weichen daher leicht ab.")
	}
	fmt.Fprintf(out, "\nPlaner-Modi (stündlich): %v\n", planned.Plans)
	return nil
}

// runSlice runs the planned strategy on a month while the forecaster still
// learns from all data before that month.
func runSlice(all, month series.Series, _ *sim.Planned, p sim.Params) sim.Result {
	startIdx := 0
	for i, r := range all {
		if r.Time.Equal(month[0].Time) {
			startIdx = i
			break
		}
	}
	window := all[:startIdx+len(month)]
	pl := sim.NewPlanned(window, p)
	// warm up the model silently by asking for limits before the month
	for i := 0; i < startIdx; i += 96 {
		pl.Limit(i, 0.5)
	}
	return sim.Run(month, offset{pl, startIdx}, p)
}

type offset struct {
	pl  *sim.Planned
	off int
}

func (o offset) Name() string                     { return o.pl.Name() }
func (o offset) Limit(i int, soc float64) float64 { return o.pl.Limit(i+o.off, soc) }

func splitMonths(s series.Series) []series.Series {
	var out []series.Series
	var cur series.Series
	for _, r := range s {
		if len(cur) > 0 && r.Time.Month() != cur[0].Time.Month() {
			out = append(out, cur)
			cur = nil
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func printResults(out io.Writer, rs []sim.Result) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	row := func(label string, f func(sim.Result) string) {
		fmt.Fprint(tw, label)
		for _, r := range rs {
			fmt.Fprint(tw, "\t", f(r))
		}
		fmt.Fprintln(tw, "\t")
	}
	row("", func(r sim.Result) string { return r.Strategy })
	row("Netzbezug", func(r sim.Result) string { return fmt.Sprintf("%.0f kWh", r.ImportKWh) })
	row("Einspeisung", func(r sim.Result) string { return fmt.Sprintf("%.0f kWh", r.ExportKWh) })
	row("  davon bei negativem Preis", func(r sim.Result) string { return fmt.Sprintf("%.0f kWh", r.ExportNegKWh) })
	row("Batterie geladen bei neg. Preis", func(r sim.Result) string { return fmt.Sprintf("%.0f kWh", r.ChargedNegKWh) })
	row("Autarkie", func(r sim.Result) string { return fmt.Sprintf("%.1f %%", 100*r.SelfSufficiency) })
	row("Eigenverbrauch", func(r sim.Result) string { return fmt.Sprintf("%.1f %%", 100*r.SelfConsumption) })
	row("Stunden mit Ladestand ≥ 90 %", func(r sim.Result) string { return fmt.Sprintf("%.0f h", r.HoursAbove90) })
	row("Stunden mit voller Batterie", func(r sim.Result) string { return fmt.Sprintf("%.0f h", r.HoursFull) })
	row("Mittlerer Ladestand", func(r sim.Result) string { return fmt.Sprintf("%.0f %%", 100*r.MeanSoC) })
	row("Ladestand um 7 Uhr (Mittel / P10)", func(r sim.Result) string {
		return fmt.Sprintf("%.0f / %.0f %%", 100*r.MorningSoCMean, 100*r.MorningSoCP10)
	})
	row("Nächte mit leerer Batterie", func(r sim.Result) string { return fmt.Sprintf("%d", r.ShortNights) })
	row("Batteriedurchsatz", func(r sim.Result) string { return fmt.Sprintf("%.0f kWh", r.ThroughputKWh) })
	row("Stromkosten netto", func(r sim.Result) string { return fmt.Sprintf("%.0f €", r.CostEUR) })
	tw.Flush()
}

func synth(args []string) error {
	fs := flag.NewFlagSet("synth", flag.ExitOnError)
	outPath := fs.String("out", "synthetic.csv", "output CSV")
	start := fs.String("start", "2026-01-01", "start date")
	days := fs.Int("days", 365, "number of days")
	kwp := fs.Float64("kwp", 10, "PV peak power")
	seed := fs.Int64("seed", 1, "random seed")
	fs.Parse(args)
	loc, _ := time.LoadLocation("Europe/Berlin")
	st, err := time.ParseInLocation("2006-01-02", *start, loc)
	if err != nil {
		return err
	}
	cal, _ := calendar.New()
	sc := sim.DefaultSynth()
	sc.KWp, sc.Seed = *kwp, *seed
	return writeSeries(*outPath, sim.Synthesize(st, *days, sc, cal))
}

func importHA(args []string) error {
	fs := flag.NewFlagSet("import-ha", flag.ExitOnError)
	in := fs.String("in", "", "Home Assistant history export (CSV)")
	outPath := fs.String("out", "history.csv", "output CSV")
	pv := fs.String("pv", "", "PV power entities, comma separated")
	load := fs.String("load", "", "house load entity (optional)")
	grid := fs.String("grid", "", "grid power entity (+import), used if -load is empty")
	bat := fs.String("battery", "", "battery power entity (+discharge), used if -load is empty")
	temp := fs.String("temp", "", "outdoor temperature entity (optional)")
	scale := fs.Float64("scale", 1, "power scale (1 = W, 1000 = kW)")
	tz := fs.String("tz", "Europe/Berlin", "time zone")
	fs.Parse(args)
	if *in == "" || *pv == "" {
		return errors.New("-in and -pv are required")
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return err
	}
	f, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer f.Close()
	s, err := importer.ConvertHA(bufio.NewReader(f), importer.Mapping{
		PV: strings.Split(*pv, ","), Load: *load, Grid: *grid, Battery: *bat, Temp: *temp, Scale: *scale,
	}, loc)
	if err != nil {
		return err
	}
	fmt.Printf("%d Viertelstunden importiert (%.0f Tage)\n", len(s), float64(len(s))/96)
	return writeSeries(*outPath, s)
}

func writeSeries(path string, s series.Series) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if err := series.WriteCSV(w, s); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
