package http

import (
	"encoding/json"
	"fmt"

	"github.com/Artiffusion-Inc/9gouter/internal/usecase/imageproxy"
)

// imageCapability is one row of the default-deny image capability table. It
// matches a (provider, model) pair and lists the exact extended fields the
// matched provider/model accepts. Fields not in the row are rejected before
// the executor with 400 (default-deny, per the image-provider-parity spec).
//
// The table is evaluated by imageCapabilities(provider, model). The first
// matching row wins; rows are ordered most-specific first. A row with an empty
// matchModels matches every model for that provider.
type imageCapability struct {
	provider    string
	matchModels []string // exact bare-model predicates; empty = any model for this provider
	allowImage  bool     // image/images inputs accepted
	allowMask   bool     // one mask alias accepted (inpainting)
	allowDims   bool     // width/height accepted
	allowNamed6 bool     // negative_prompt/guidance/seed/num_steps/steps/strength accepted
}

// cloudflareMultipartModels are the three legacy FLUX.2 multipart models that
// accept dimensions + named fields but no image/mask. The canonical list lives
// in the imageproxy Cloudflare adapter (imageproxy.CloudflareMultipartModels)
// so the usecase and the capability table share one source of truth.
var cloudflareMultipartModels = imageproxy.CloudflareMultipartModels

// cloudflareImg2ImgModel is the single Cloudflare img2img JSON model.
const cloudflareImg2ImgModel = imageproxy.CloudflareImg2ImgModel

// cloudflareInpaintingModel is the single Cloudflare inpainting JSON model.
const cloudflareInpaintingModel = imageproxy.CloudflareInpaintingModel

// imageCapabilities resolves the capability row for a (provider, model) pair.
// It returns (row, true) when the provider is known to the image matrix and a
// row applies; (zero, false) when the provider has no row at all (the caller
// decides whether to reject all extended fields or treat the provider as
// unknown). Default-deny: any field not authorised by the returned row is
// rejected by checkImageCapabilities.
func imageCapabilities(provider, model string) (imageCapability, bool) {
	switch provider {
	case "fal-ai":
		// fal-ai accepts one image input → image_url.
		return imageCapability{provider: provider, allowImage: true}, true
	case "black-forest-labs":
		// Only flux-kontext-pro / flux-kontext-max accept an image (image_prompt).
		if model == "flux-kontext-pro" || model == "flux-kontext-max" {
			return imageCapability{provider: provider, allowImage: true}, true
		}
		// Other BFL models accept no extended fields.
		return imageCapability{provider: provider}, true
	case "cloudflare-ai":
		// Multipart models: dimensions + named fields, no image/mask.
		if isCloudflareMultipartModel(model) {
			return imageCapability{provider: provider, allowDims: true, allowNamed6: true}, true
		}
		// Inpainting: image + one mask + dimensions + named fields.
		if model == cloudflareInpaintingModel {
			return imageCapability{provider: provider, allowImage: true, allowMask: true, allowDims: true, allowNamed6: true}, true
		}
		// Img2img: image + dimensions + named fields.
		if model == cloudflareImg2ImgModel {
			return imageCapability{provider: provider, allowImage: true, allowDims: true, allowNamed6: true}, true
		}
		// Other Cloudflare JSON image models: dimensions + named fields only.
		return imageCapability{provider: provider, allowDims: true, allowNamed6: true}, true
	case "runwayml", "nanobanana":
		// Edit pass-through intentionally excluded from this Go parity tranche.
		// Text-to-image still proceeds; any extended image/mask field is rejected.
		return imageCapability{provider: provider}, true
	case "sdwebui", "comfyui":
		// Local no-auth providers: the spec decision table (step 5) fixes their
		// accepted fields. For step 3 the capability row exists so that the
		// presence-bearing extended fields are not silently dropped; step 5
		// will tighten the accepted set per the SDWebUI decision table. Until
		// then no extended fields are authorised (default-deny).
		return imageCapability{provider: provider}, true
	}
	// Providers not in the matrix (openai, gemini, codex, etc.) have no extended
	// fields: every supplied extended field is rejected.
	return imageCapability{}, false
}

func isCloudflareMultipartModel(model string) bool {
	for _, m := range cloudflareMultipartModels {
		if m == model {
			return true
		}
	}
	return false
}

// capabilityError is the 400 message shape mandated by the spec:
// `<provider> does not support <field> for this image model`.
func capabilityError(provider, field string) error {
	return fmt.Errorf("%s does not support %s for this image model", provider, field)
}

// checkImageCapabilities enforces the default-deny capability table against the
// supplied (presence-bearing) extended fields. It returns a non-nil error
// (caller writes 400) when any supplied field is not authorised by the row.
//
// Presence semantics: a field is "supplied" when its json.RawMessage is non-nil
// (the key was present in the request body), regardless of whether the value is
// null, "", or 0. This is the exact spec rule: "Любой key, включая null и 0,
// считается supplied для capability check."
//
// maskAliases is the count of supplied mask aliases (mask_image/maskImage/mask);
// the caller canonicalises them before this check and passes the count so this
// function only needs to know whether a canonical mask was supplied.
func checkImageCapabilities(provider, model string, cap imageCapability, supplied suppliedImageFields, maskSupplied bool) error {
	if supplied.image != nil {
		if !cap.allowImage {
			return capabilityError(provider, "image")
		}
	}
	if supplied.images != nil {
		if !cap.allowImage {
			return capabilityError(provider, "images")
		}
	}
	if maskSupplied {
		if !cap.allowMask {
			return capabilityError(provider, "mask")
		}
	}
	if supplied.width != nil {
		if !cap.allowDims {
			return capabilityError(provider, "width")
		}
	}
	if supplied.height != nil {
		if !cap.allowDims {
			return capabilityError(provider, "height")
		}
	}
	if supplied.negativePrompt != nil {
		if !cap.allowNamed6 {
			return capabilityError(provider, "negative_prompt")
		}
	}
	if supplied.guidance != nil {
		if !cap.allowNamed6 {
			return capabilityError(provider, "guidance")
		}
	}
	if supplied.seed != nil {
		if !cap.allowNamed6 {
			return capabilityError(provider, "seed")
		}
	}
	if supplied.numSteps != nil {
		if !cap.allowNamed6 {
			return capabilityError(provider, "num_steps")
		}
	}
	if supplied.steps != nil {
		if !cap.allowNamed6 {
			return capabilityError(provider, "steps")
		}
	}
	if supplied.strength != nil {
		if !cap.allowNamed6 {
			return capabilityError(provider, "strength")
		}
	}
	return nil
}

// suppliedImageFields carries the presence-bearing raw JSON of each extended
// field. nil = key absent; non-nil = key supplied (including null/""/0).
type suppliedImageFields struct {
	image          json.RawMessage
	images         json.RawMessage
	width          json.RawMessage
	height         json.RawMessage
	negativePrompt json.RawMessage
	guidance       json.RawMessage
	seed           json.RawMessage
	numSteps       json.RawMessage
	steps          json.RawMessage
	strength       json.RawMessage
	maskImage      json.RawMessage // mask_image alias
	maskCamel      json.RawMessage // maskImage alias
	mask           json.RawMessage // mask alias
}

// present reports whether a json.RawMessage field is supplied (non-nil).
func present(v json.RawMessage) bool { return v != nil }

// canonicalizeMask validates that at most one of the three mask aliases
// (mask_image, maskImage, mask) is supplied and returns the canonical raw
// value plus a flag. Two or more supplied aliases is an explicit 400. A supplied
// alias counts even when its value is null or empty (presence semantics).
//
// The returned raw value is passed to the safe input resolver (step 4); the
// capability check (checkImageCapabilities) only needs to know that a mask was
// supplied (maskSupplied=true).
func canonicalizeMask(f suppliedImageFields) (json.RawMessage, bool, error) {
	n := 0
	var canonical json.RawMessage
	if present(f.maskImage) {
		n++
		canonical = f.maskImage
	}
	if present(f.maskCamel) {
		n++
		if n == 1 {
			canonical = f.maskCamel
		}
	}
	if present(f.mask) {
		n++
		if n == 1 {
			canonical = f.mask
		}
	}
	if n > 1 {
		return nil, false, fmt.Errorf("conflicting mask aliases: supply at most one of mask_image, maskImage, mask")
	}
	if n == 0 {
		return nil, false, nil
	}
	return canonical, true, nil
}

// rawImageInputs returns the raw image inputs (image and/or images) as a slice
// for the safe input resolver (step 4). When neither is supplied it returns nil.
// The capability check has already authorised the fields; here we only collect
// them. `images` may be a JSON array or a single value; the resolver normalises
// both. For step 3 the handler does not yet call the resolver; it only forwards
// the raw values to Options so the stub/adapter probe can observe them.
func rawImageInputs(f suppliedImageFields) []json.RawMessage {
	var out []json.RawMessage
	if present(f.image) {
		out = append(out, f.image)
	}
	if present(f.images) {
		out = append(out, f.images)
	}
	return out
}

// suppress unused warning helper kept for future use when the resolver
// consumes raw inputs.
var _ = canonicalizeMask
