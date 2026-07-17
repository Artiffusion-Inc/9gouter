package proxychat

import (
	"encoding/json"
	"fmt"

	"github.com/Artiffusion-Inc/9router/internal/adapter/rtk"
	"github.com/Artiffusion-Inc/9router/internal/domain/format"
)

// runRtk runs the RTK compressMessages stage when enabled.
func runRtk(body map[string]any, cfg TokenSaverConfig) *rtk.Stats {
	if !cfg.RtkEnabled {
		return nil
	}
	rtk.SetEnabled(true)
	return rtk.CompressMessages(body, true, nil)
}

// formatRtkLog delegates to the rtk package formatter.
func formatRtkLog(stats *rtk.Stats) string {
	return rtk.FormatRtkLog(stats)
}

// runHeadroom runs the optional Headroom proxy compression.
func runHeadroom(body map[string]any, cfg TokenSaverConfig, providerID string, targetFormat format.Format, log logger) *headroomResult {
	if !cfg.HeadroomEnabled || cfg.HeadroomURL == "" {
		return nil
	}
	diag := &rtk.HeadroomDiagnostics{}
	stats, err := rtk.CompressWithHeadroom(body, rtk.HeadroomConfig{
		Enabled:              true,
		URL:                  cfg.HeadroomURL,
		Model:                providerID,
		Format:               targetFormat,
		CompressUserMessages: cfg.HeadroomCompressUser,
		TimeoutMs:            3000,
		Diagnostics:          diag,
	})
	if err != nil || stats == nil {
		return &headroomResult{skippedReason: diag.Reason}
	}
	res := &headroomResult{
		saved:       stats.TokensSaved,
		log:         rtk.FormatHeadroomLog(stats),
		sizeLog:     rtk.FormatHeadroomSizeLog(diag),
		messagesLog: headroomMessagesLog(stats.Messages),
		phantom:     rtk.IsHeadroomPhantomSavings(stats, diag, 0.05),
		tokensBefore: stats.TokensBefore,
		tokensAfter:  stats.TokensAfter,
	}
	return res
}

func headroomMessagesLog(msgs []rtk.HeadroomMessage) string {
	if len(msgs) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(msgs)
	return string(b)
}

// runPxpipe runs the optional pxpipe image-compression stage.
type pxpipeRunResult struct {
	body    map[string]any
	summary *rtk.PxpipeSummary
}

func runPxpipe(body map[string]any, targetFormat format.Format, model string, cfg TokenSaverConfig) pxpipeRunResult {
	if !cfg.PxpipeEnabled || cfg.PxpipeTransform == nil {
		return pxpipeRunResult{body: body, summary: nil}
	}
	transform := func(args rtk.PxpipeTransformArgs) (*rtk.PxpipeTransformResult, error) {
		out, err := cfg.PxpipeTransform(args.Body, args.Model, args.MinCompressChars)
		if err != nil {
			return nil, err
		}
		return &rtk.PxpipeTransformResult{Applied: true, Body: out}, nil
	}
	res := rtk.CompressWithPxpipe(body, targetFormat, model, cfg.PxpipeMinChars, cfg.PxpipeTimeoutMs, transform)
	if res == nil || res.Body == nil {
		return pxpipeRunResult{body: body, summary: res.Summary}
	}
	return pxpipeRunResult{body: res.Body, summary: res.Summary}
}

// injectCaveman and injectPonytail wrap the rtk injectors.
func injectCaveman(body map[string]any, targetFormat format.Format, level string) {
	rtk.InjectCaveman(body, targetFormat, level)
}

func injectPonytail(body map[string]any, targetFormat format.Format, level string) {
	rtk.InjectPonytail(body, targetFormat, level)
}

func formatPxpipeSummary(summary *rtk.PxpipeSummary) string {
	if summary == nil {
		return ""
	}
	return fmt.Sprintf("PXPIPE:%dimg", summary.ImageCount)
}
