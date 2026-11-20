// nvrfetch downloads NVR footage over ONVIF (Profile G) as MP4.
//
//	nvrfetch -ip ... -start ... -end ...   one-off download (CLI)
//	nvrfetch serve -addr :8080             HTTP API
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"

	"com.flipkart.nvrvms/internal/api"
	"com.flipkart.nvrvms/internal/footage"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.SetFlags(log.Ltime)

	var err error
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		err = serve(ctx, os.Args[2:])
	} else {
		err = fetch(ctx, os.Args[1:])
	}
	if err != nil {
		log.Fatal(err)
	}
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var cfg api.Config
	addr := fs.String("addr", ":8080", "listen address")
	fs.DurationVar(&cfg.MaxDuration, "max-duration", time.Hour, "max footage range per request")
	fs.IntVar(&cfg.MaxConcurrent, "max-concurrent", 4, "max simultaneous fetches")
	fs.StringVar(&cfg.APIKey, "api-key", os.Getenv("NVR_API_KEY"), "require this key in X-API-Key header (or env NVR_API_KEY)")
	fs.StringVar(&cfg.TmpDir, "tmp-dir", "", "staging dir for MP4s (default OS temp)")
	_ = fs.Parse(args)

	if cfg.APIKey == "" {
		log.Print("warning: no API key set; anyone who can reach this port can make the server connect to arbitrary hosts")
	}
	sc := echo.StartConfig{Address: *addr, GracefulTimeout: 30 * time.Second}
	return sc.Start(ctx, api.New(cfg))
}

func fetch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("nvrfetch", flag.ExitOnError)
	r := footage.Request{Stderr: os.Stderr}
	list := fs.Bool("list", false, "list recordings (channels) on the NVR and exit")
	fs.StringVar(&r.Host, "ip", "", "NVR IP address or hostname (required)")
	fs.IntVar(&r.Port, "port", 80, "NVR ONVIF HTTP port")
	fs.StringVar(&r.Username, "user", "admin", "NVR username")
	fs.StringVar(&r.Password, "pass", "", "NVR password (or env NVR_PASSWORD)")
	fs.IntVar(&r.Channel, "channel", 1, "camera channel number (1-based)")
	fs.StringVar(&r.RecordingToken, "recording", "", "ONVIF recording token (overrides -channel; see -list)")
	fs.StringVar(&r.Start, "start", "", `start local time, "YYYY-MM-DD HH:MM:SS"`)
	fs.StringVar(&r.End, "end", "", `end local time, "YYYY-MM-DD HH:MM:SS"`)
	fs.StringVar(&r.TZ, "tz", "", `timezone of -start/-end: "" = NVR's timezone, "Local", or IANA name e.g. Asia/Kolkata`)
	fs.StringVar(&r.Output, "out", "", "output .mp4 path (default footage_ch<N>_<start>.mp4)")
	fs.BoolVar(&r.Fast, "fast", false, "ask NVR to stream faster than real time (not supported by all NVRs)")
	fs.StringVar(&r.FFmpeg, "ffmpeg", "ffmpeg", "ffmpeg binary")
	_ = fs.Parse(args)

	if r.Password == "" {
		r.Password = os.Getenv("NVR_PASSWORD")
	}
	if r.Host == "" || (!*list && (r.Start == "" || r.End == "")) {
		fs.Usage()
		os.Exit(2)
	}

	if *list {
		recs, err := footage.List(ctx, r)
		if err != nil {
			return err
		}
		for i, rec := range recs {
			fmt.Printf("channel %-3d token=%q source=%q sourceId=%q %s\n", i+1, rec.Token, rec.SourceName, rec.SourceID, rec.Description)
		}
		return nil
	}

	if r.Output == "" {
		r.Output = fmt.Sprintf("footage_ch%d_%s.mp4", r.Channel, strings.NewReplacer("-", "", ":", "", " ", "_", "T", "_").Replace(r.Start))
	}
	if err := footage.Fetch(ctx, r); err != nil {
		return err
	}
	log.Printf("saved %s", r.Output)
	return nil
}
