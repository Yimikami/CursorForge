package relay

import "google.golang.org/protobuf/encoding/protowire"

// buildGetDefaultModelNudgeDataResponse mirrors the working app's reply to
// /aiserver.v1.AiService/GetDefaultModelNudgeData. Capture format (21 B):
//
//   0a 01 30                       field 1 (bytes), len 1, value "0"
//   1a 10 <16 ASCII hex bytes>     field 3 (bytes), len 16, model_id hex
//
// Field 1 looks like a category / nudge_type (the literal character "0", not
// the numeric zero) and field 3 is the recommended default model. Cursor's
// chat picker queries this on every open and shows nothing if the call fails
// — returning the BYOK hex here makes the picker accept our model as the
// suggested default.
func buildGetDefaultModelNudgeDataResponse(adapters []AdapterInfo) []byte {
	if len(adapters) == 0 {
		return nil
	}
	hexID := adapters[0].StableID()
	out := make([]byte, 0, 32)
	out = appendString(out, 1, "0")
	out = appendString(out, 3, hexID)
	return out
}

// buildAvailableModelsResponse hand-rolls the protobuf bytes for an
// AvailableModelsResponse that mirrors the closed-source working app's
// 360-byte payload exactly, but with one entry per BYOK adapter.
//
// The shape was reverse-engineered from a captured response. Cursor's chat
// picker is sensitive to which fields are present and which aren't; the
// generated proto types we ship cover only fields documented in the public
// burpheart extract. Anything beyond that (fields 12, 13, 38) is appended
// as raw varints with the same values the working app sends.
func buildAvailableModelsResponse(adapters []AdapterInfo) []byte {
	buf := make([]byte, 0, 512)

	for _, a := range adapters {
		if a.ModelID == "" {
			continue
		}
		hexID := a.StableID()
		display := a.DisplayName
		if display == "" {
			display = a.ModelID
		}
		// AvailableModel (repeated, field 2)
		modelBytes := buildAvailableModel(hexID, display)
		buf = protowire.AppendTag(buf, protowire.Number(2), protowire.BytesType)
		buf = protowire.AppendVarint(buf, uint64(len(modelBytes)))
		buf = append(buf, modelBytes...)
	}

	if len(adapters) == 0 {
		return buf
	}
	first := adapters[0]
	hexID := first.StableID()

	// FeatureModelConfig fields. Working app populates all of:
	//   4 composer_model_config        (default + fallback + best_of_n)
	//   5 cmd_k_model_config           (default + fallback)
	//   6 background_composer_model_config (default + fallback + best_of_n)
	//   7 plan_execution_model_config  (default + fallback)
	//   8 spec_model_config            (default only)
	//   9 deep_search_model_config     (default only)
	//  10 quick_agent_model_config     (default only)
	buf = appendFeatureCfg(buf, 4, hexID, true, true)
	buf = appendFeatureCfg(buf, 5, hexID, true, false)
	buf = appendFeatureCfg(buf, 6, hexID, true, true)
	buf = appendFeatureCfg(buf, 7, hexID, true, false)
	buf = appendFeatureCfg(buf, 8, hexID, false, false)
	buf = appendFeatureCfg(buf, 9, hexID, false, false)
	buf = appendFeatureCfg(buf, 10, hexID, false, false)

	// Fields 12 (= 2400000) and 13 (= 2). Unknown semantics but the working
	// app emits these every time. Replicating them keeps Cursor's picker
	// from flipping into the "missing data, hide BYOK" branch. The exact
	// value 2,400,000 was decoded from a captured working-app response
	// (varint bytes 80 BE 92 01).
	buf = protowire.AppendTag(buf, protowire.Number(12), protowire.VarintType)
	buf = protowire.AppendVarint(buf, 2400000)
	buf = protowire.AppendTag(buf, protowire.Number(13), protowire.VarintType)
	buf = protowire.AppendVarint(buf, 2)

	return buf
}

// buildAvailableModel constructs one AvailableModel sub-message body matching
// the working app's exact field selection.
func buildAvailableModel(hexID, display string) []byte {
	out := make([]byte, 0, 128)
	// 1: name (bytes) = hexID
	out = appendString(out, 1, hexID)
	// 2: default_on (varint) = true
	out = appendBool(out, 2, true)
	// 5: supports_agent (varint) = true
	out = appendBool(out, 5, true)
	// 6: degradation_status (varint) = 0 (UNSPECIFIED)
	out = protowire.AppendTag(out, protowire.Number(6), protowire.VarintType)
	out = protowire.AppendVarint(out, 0)
	// 8: tooltip_data — a TooltipData sub-message containing only field 7
	// (tertiary_text) = "Notes". Working app sets this verbatim.
	tooltip := append([]byte(nil), 0x3a, 0x05)
	tooltip = append(tooltip, []byte("Notes")...)
	out = protowire.AppendTag(out, protowire.Number(8), protowire.BytesType)
	out = protowire.AppendVarint(out, uint64(len(tooltip)))
	out = append(out, tooltip...)
	// 9: supports_thinking (varint) = true
	out = appendBool(out, 9, true)
	// 10: supports_images (varint) = true
	out = appendBool(out, 10, true)
	// 14: supports_max_mode (varint) = true
	out = appendBool(out, 14, true)
	// 17: client_display_name (bytes) = display
	out = appendString(out, 17, display)
	// 18: server_model_name (bytes) = hexID (NOT the user's modelID!)
	out = appendString(out, 18, hexID)
	// 19: supports_non_max_mode (varint) = true
	out = appendBool(out, 19, true)
	// 20: tooltip_data_for_max_mode — same Notes tooltip
	out = protowire.AppendTag(out, protowire.Number(20), protowire.BytesType)
	out = protowire.AppendVarint(out, uint64(len(tooltip)))
	out = append(out, tooltip...)
	// 21: is_recommended_for_background_composer = false (still emit it)
	out = appendBool(out, 21, false)
	// 22: supports_plan_mode (varint) = true
	out = appendBool(out, 22, true)
	// 24: inputbox_short_model_name (bytes) = display
	out = appendString(out, 24, display)
	// 25: supports_sandboxing (varint) = true
	out = appendBool(out, 25, true)
	// 38: BYOK-eligible flag (varint = 1). Working app emits this on every
	// AvailableModel — without it Cursor's picker filters the model out as
	// "not eligible for your account" even though the outer Pro flags are
	// set. Decoded byte-for-byte from the working app's 360-byte response
	// (tag 0xb0 0x02 = field 38 varint, value 0x01).
	out = appendBool(out, 38, true)
	return out
}

// appendFeatureCfg appends a FeatureModelConfig sub-message to buf at the
// outer field number `field`. The model_id is replicated into default_model,
// fallback_models[0], and (when withBestOfN) best_of_n_default_models[0].
func appendFeatureCfg(buf []byte, field int, hexID string, withFallback, withBestOfN bool) []byte {
	cfg := make([]byte, 0, 64)
	// 1: default_model (bytes)
	cfg = appendString(cfg, 1, hexID)
	if withFallback {
		// 2: fallback_models (repeated bytes)
		cfg = appendString(cfg, 2, hexID)
	}
	if withBestOfN {
		// 3: best_of_n_default_models (repeated bytes)
		cfg = appendString(cfg, 3, hexID)
	}
	buf = protowire.AppendTag(buf, protowire.Number(field), protowire.BytesType)
	buf = protowire.AppendVarint(buf, uint64(len(cfg)))
	return append(buf, cfg...)
}

func appendString(buf []byte, field int, value string) []byte {
	buf = protowire.AppendTag(buf, protowire.Number(field), protowire.BytesType)
	buf = protowire.AppendString(buf, value)
	return buf
}

func appendBool(buf []byte, field int, v bool) []byte {
	buf = protowire.AppendTag(buf, protowire.Number(field), protowire.VarintType)
	if v {
		return protowire.AppendVarint(buf, 1)
	}
	return protowire.AppendVarint(buf, 0)
}
