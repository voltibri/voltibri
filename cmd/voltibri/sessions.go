// Copyright 2026 The voltibri Authors
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/voltibri/voltibri/internal/config"
	"github.com/voltibri/voltibri/internal/importer"
	"github.com/voltibri/voltibri/internal/run"
	"github.com/voltibri/voltibri/internal/store"
)

const sessionsUsage = `Usage:
  voltibri runs list         [-data DIR] [-n 20] [-asset ID] [-kind charging|heating|run]
  voltibri runs import-evcc  [-data DIR] -url http://evcc:7070 [-map "Carport=wallbox-carport,Zaun=wallbox-zaun"] [-dry-run]
  voltibri runs imports      [-data DIR]
  voltibri runs undo-import  [-data DIR] ID

Runs are charging sessions, heating cycles and other activities of a
device. Imported runs are marked with their source and import; undo-import
removes exactly the runs of one import. Runs imported before are skipped,
so import-evcc can be repeated until evcc is switched off. ("sessions" is
an alias of "runs".)
`

func sessionsCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(sessionsUsage)
		return errors.New("subcommand missing")
	}
	fs := flag.NewFlagSet("sessions "+args[0], flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "data directory, env VOLTIBRI_DATA")
	url := fs.String("url", "", "evcc address, e.g. http://evcc.local:7070")
	mapping := fs.String("map", "", "evcc loadpoint title=voltibri charger id, comma separated")
	dry := fs.Bool("dry-run", false, "only show what would be imported")
	n := fs.Int("n", 20, "number of runs")
	asset := fs.String("asset", "", "only runs of this device")
	all := fs.Bool("all", false, "include measured runs hidden by an overlapping import")
	kind := fs.String("kind", "", "only runs of this kind: charging, heating, run")
	fs.Parse(args[1:])
	st, err := store.Open(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	defer tw.Flush()

	switch args[0] {
	case "list":
		ss, err := st.Runs(ctx, run.Filter{Asset: *asset, Kind: *kind, Limit: *n, All: *all})
		if err != nil {
			return err
		}
		fmt.Fprintln(tw, "Beginn\tGerät\tArt\tFahrzeug\tEnergie\tPV/Akku\tKosten\tQuelle")
		for _, s := range ss {
			src, cost := "voltibri", fmt.Sprintf("%.2f €", s.CostCt/100)
			if s.Source != "" {
				src = fmt.Sprintf("%s (Import %d)", s.Source, s.ImportID)
			}
			if s.CostUnknown {
				cost = "–"
			}
			share := "–"
			if s.EnergyWh > 0 {
				share = fmt.Sprintf("%.0f %%", 100*s.SolarWh()/s.EnergyWh)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.1f kWh\t%s\t%s\t%s\n", s.Start.Format("02.01.2006 15:04"), s.Asset, s.Kind, s.VehicleName, s.EnergyWh/1000, share, cost, src)
		}
		return nil

	case "import-evcc":
		if *url == "" {
			return errors.New("-url is required")
		}
		m := map[string]string{}
		for _, kv := range strings.Split(*mapping, ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				m[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		cfg, _, err := st.Current(ctx)
		if errors.Is(err, store.ErrNoConfig) {
			cfg, err = config.Default(), nil
		}
		if err != nil {
			return err
		}
		list, err := importer.FetchEvcc(ctx, *url)
		if err != nil {
			return err
		}
		known, err := st.ImportedIDs(ctx, "evcc")
		if err != nil {
			return err
		}
		p := importer.PlanEvcc(list, known, importer.Assets(cfg), m)
		fmt.Printf("evcc liefert %d Einträge: %d neu, %d schon importiert, %d laufen noch, %d ausgelassen.\n", p.Total, p.New, p.Duplicate, p.Unfinished, p.Excluded)
		if p.New > 0 {
			fmt.Printf("Neu: %s bis %s, %.1f kWh, %d mit Preis.\n", p.From.Format("02.01.2006"), p.To.Format("02.01.2006"), p.EnergyWh/1000, p.WithPrice)
		}
		params := map[string]any{"url": *url}
		mp := map[string]any{}
		for _, lm := range p.Loadpoints {
			target, how := lm.Target, "angegeben"
			switch {
			case target == importer.Skip:
				target, how = "(nicht übernehmen)", "kein passendes Gerät – mit -map zuordnen, = behält den evcc-Namen"
			case target == importer.Keep:
				target, how = lm.Title, "evcc-Name"
			case lm.Auto:
				how = "per Name erkannt"
			}
			fmt.Printf("  %-22s → %-22s %4d Läufe %-9s (%s)\n", lm.Title, target, lm.Sessions, lm.RunKind, how)
			mp[lm.Title] = lm.Target
		}
		params["mapping"] = mp
		if *dry || p.New == 0 {
			return nil
		}
		imp, _, err := st.ImportRuns(ctx, store.Import{Source: "evcc", Author: "cli", Description: strconv.Itoa(p.New) + " Läufe aus evcc", Params: params}, p.Runs)
		if err != nil {
			return err
		}
		fmt.Printf("Import %d: %d Läufe übernommen. Rückgängig: voltibri runs undo-import %d\n", imp.ID, imp.Count, imp.ID)
		return nil

	case "imports":
		list, err := st.Imports(ctx)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("Noch keine Importe.")
			return nil
		}
		fmt.Fprintln(tw, "Nr.\tZeit\tQuelle\tAnzahl\tvon\tStatus")
		for _, i := range list {
			status := "aktiv"
			if i.UndoneAt != nil {
				status = "rückgängig gemacht " + i.UndoneAt.Format("02.01.2006 15:04")
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%s\t%s\n", i.ID, i.CreatedAt.Format("02.01.2006 15:04"), i.Source, i.Count, i.Author, status)
		}
		return nil

	case "undo-import":
		if fs.NArg() != 1 {
			return errors.New("usage: voltibri runs undo-import ID")
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil {
			return err
		}
		removed, err := st.UndoImport(ctx, id)
		if err != nil {
			return err
		}
		fmt.Printf("Import %d rückgängig gemacht: %d Ladevorgänge entfernt.\n", id, removed)
		return nil
	}
	fmt.Print(sessionsUsage)
	return fmt.Errorf("unknown subcommand %q", args[0])
}
