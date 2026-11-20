// Package api exposes footage fetching over HTTP (Echo v5).
package api

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"com.flipkart.nvrvms/internal/footage"
)

type Config struct {
	MaxDuration   time.Duration // longest footage range per request
	MaxConcurrent int           // simultaneous fetches (each runs one ffmpeg)
	APIKey        string        // if set, required in X-API-Key header
	TmpDir        string        // where MP4s are staged; "" = OS temp dir
}

type nvrParams struct {
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type footageRequest struct {
	nvrParams
	Channel        int    `json:"channel"`
	RecordingToken string `json:"recording_token"`
	Start          string `json:"start"` // "YYYY-MM-DD HH:MM:SS", local time
	End            string `json:"end"`
	TZ             string `json:"tz"` // "" = NVR timezone, "Local", or IANA name
	Fast           bool   `json:"fast"`
}

type recordingDTO struct {
	Channel     int    `json:"channel"`
	Token       string `json:"token"`
	SourceName  string `json:"source_name"`
	SourceID    string `json:"source_id"`
	Description string `json:"description,omitempty"`
}

type server struct {
	cfg Config
	sem chan struct{}
}

func New(cfg Config) *echo.Echo {
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 1
	}
	s := &server{cfg: cfg, sem: make(chan struct{}, cfg.MaxConcurrent)}

	e := echo.New()
	e.Use(middleware.RequestID(), middleware.RequestLogger(), middleware.Recover())
	e.GET("/healthz", func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) })

	g := e.Group("/api/v1")
	if cfg.APIKey != "" {
		g.Use(middleware.KeyAuthWithConfig(middleware.KeyAuthConfig{
			KeyLookup: "header:X-API-Key",
			Validator: func(_ *echo.Context, key string, _ middleware.ExtractorSource) (bool, error) {
				return subtle.ConstantTimeCompare([]byte(key), []byte(cfg.APIKey)) == 1, nil
			},
		}))
	}
	g.POST("/footage", s.footage)
	g.POST("/recordings", s.recordings)
	return e
}

func (p *nvrParams) validate() error {
	if p.IP == "" || strings.ContainsAny(p.IP, "/?#@ ") {
		return echo.NewHTTPError(http.StatusBadRequest, "ip is required and must be a bare IP/hostname")
	}
	if p.Port == 0 {
		p.Port = 80
	}
	if p.Port < 1 || p.Port > 65535 {
		return echo.NewHTTPError(http.StatusBadRequest, "port must be 1-65535")
	}
	return nil
}

func (p nvrParams) request(c *echo.Context) footage.Request {
	id := c.Response().Header().Get(echo.HeaderXRequestID)
	return footage.Request{
		Host: p.IP, Port: p.Port, Username: p.Username, Password: p.Password,
		Log: slog.NewLogLogger(c.Logger().With("request_id", id).Handler(), slog.LevelInfo),
	}
}

// POST /api/v1/footage → video/mp4 attachment.
func (s *server) footage(c *echo.Context) error {
	var in footageRequest
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if err := in.validate(); err != nil {
		return err
	}
	if in.Channel == 0 && in.RecordingToken == "" {
		in.Channel = 1
	}
	if in.Start == "" || in.End == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "start and end are required")
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return echo.NewHTTPError(http.StatusTooManyRequests, "too many concurrent fetches, retry later")
	}

	dir, err := os.MkdirTemp(s.cfg.TmpDir, "nvrfetch-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	req := in.request(c)
	req.Channel, req.RecordingToken = in.Channel, in.RecordingToken
	req.Start, req.End, req.TZ, req.Fast = in.Start, in.End, in.TZ, in.Fast
	req.MaxDuration = s.cfg.MaxDuration
	req.Output = filepath.Join(dir, "footage.mp4")

	if err := footage.Fetch(c.Request().Context(), req); err != nil {
		return mapErr(err)
	}

	stamp := strings.NewReplacer("-", "", ":", "", " ", "_", "T", "_").Replace(in.Start)
	name := fmt.Sprintf("ch%d_%s.mp4", in.Channel, stamp)
	if in.RecordingToken != "" {
		name = "footage_" + stamp + ".mp4"
	}
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, "video/mp4")
	h.Set(echo.HeaderContentDisposition, `attachment; filename="`+name+`"`)
	return c.FileFS("footage.mp4", os.DirFS(dir))
}

// POST /api/v1/recordings → channel/recording mapping.
func (s *server) recordings(c *echo.Context) error {
	var in nvrParams
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	if err := in.validate(); err != nil {
		return err
	}
	recs, err := footage.List(c.Request().Context(), in.request(c))
	if err != nil {
		return mapErr(err)
	}
	out := make([]recordingDTO, len(recs))
	for i, r := range recs {
		out[i] = recordingDTO{Channel: i + 1, Token: r.Token, SourceName: r.SourceName, SourceID: r.SourceID, Description: r.Description}
	}
	return c.JSON(http.StatusOK, out)
}

func mapErr(err error) error {
	if errors.Is(err, footage.ErrInvalid) {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return echo.NewHTTPError(http.StatusBadGateway, err.Error())
}
