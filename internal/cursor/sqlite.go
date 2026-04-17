package cursor

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"
)

const reactiveStorageKey = "src.vs.platform.reactivestorage.browser.reactiveStorageServiceImpl.persistentStorage.applicationUser"

// fakeProSessionJWT is a hand-crafted JWT that Cursor IDE accepts as a valid
// session token. The IDE never validates the HMAC signature locally — it only
// base64-decodes the payload to render the email and "logged in" state. The
// real validation happens against api2.cursor.sh, which we MITM and answer
// with a synthetic Pro-membership response.
//
// Payload: {"sub":"fake-cursor-local-user","email":"cursor@ai.com",
//           "type":"session","iss":"cursor-client",
//           "scope":"openid profile email","exp":4070908800}
// Extracted byte-for-byte from the working closed-source app (Linux v0.0.4).
const fakeProSessionJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJmYWtlLWN1cnNvci1sb2NhbC11c2VyIiwiZW1haWwiOiJjdXJzb3JAYWkuY29tIiwidHlwZSI6InNlc3Npb24iLCJpc3MiOiJjdXJzb3ItY2xpZW50Iiwic2NvcGUiOiJvcGVuaWQgcHJvZmlsZSBlbWFpbCIsImV4cCI6NDA3MDkwODgwMH0.fake-local-state-token"

const fakeProEmail = "cursor@ai.com"

// fakeAuthKeys lists the cursorAuth/* keys we overwrite with fake-Pro
// values. We back up whatever was there before so we can restore it when
// the proxy stops — otherwise Cursor would keep sending our fake JWT to
// the real api2.cursor.sh after the user disables our proxy and get back
// ERROR_NOT_LOGGED_IN on every call.
var fakeAuthKeys = []string{
	"cursorAuth/accessToken",
	"cursorAuth/refreshToken",
	"cursorAuth/cachedEmail",
	"cursorAuth/cachedSignUpType",
	"cursorAuth/stripeMembershipType",
}

// InjectFakeProUser overwrites Cursor's cursorAuth/* keys with the fake
// Pro session and persists the previous values to backupPath. If a backup
// already exists (i.e. we ran before without a clean Restore), we leave
// the existing backup untouched so the original user data isn't lost.
func InjectFakeProUser(backupPath string) error {
	dbPath := stateDBPath()
	if dbPath == "" {
		return fmt.Errorf("could not resolve Cursor state.vscdb path")
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout=2000")
	if err != nil {
		return err
	}
	defer db.Close()

	if backupPath != "" {
		if _, statErr := os.Stat(backupPath); os.IsNotExist(statErr) {
			backup := map[string]*string{}
			alreadyFake := false
			for _, key := range fakeAuthKeys {
				var v string
				row := db.QueryRow("SELECT value FROM ItemTable WHERE key = ?", key)
				switch err := row.Scan(&v); err {
				case nil:
					vv := v
					backup[key] = &vv
					if key == "cursorAuth/accessToken" && v == fakeProSessionJWT {
						alreadyFake = true
					}
				case sql.ErrNoRows:
					backup[key] = nil // sentinel = key didn't exist
				default:
					return fmt.Errorf("backup read %s: %w", key, err)
				}
			}
			// The current SQLite already holds our fake session — most
			// likely a previous Start crashed without a clean Stop. Saving
			// these as "original" would leave the user permanently logged
			// in as cursor@ai.com after Stop, so we instead persist a
			// "delete on restore" marker for every key and let the user
			// log into Cursor manually after the next Stop.
			if alreadyFake {
				for _, key := range fakeAuthKeys {
					backup[key] = nil
				}
			}
			data, jerr := json.MarshalIndent(backup, "", "  ")
			if jerr != nil {
				return jerr
			}
			if werr := os.WriteFile(backupPath, data, 0o600); werr != nil {
				return fmt.Errorf("write backup: %w", werr)
			}
		}
	}

	pairs := [][2]string{
		{"cursorAuth/accessToken", fakeProSessionJWT},
		{"cursorAuth/refreshToken", fakeProSessionJWT},
		{"cursorAuth/cachedEmail", fakeProEmail},
		{"cursorAuth/cachedSignUpType", "Auth"},
		{"cursorAuth/stripeMembershipType", "pro"},
	}
	for _, kv := range pairs {
		if _, err := db.Exec(
			"INSERT INTO ItemTable(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			kv[0], kv[1],
		); err != nil {
			return fmt.Errorf("upsert %s: %w", kv[0], err)
		}
	}
	return nil
}

// RestoreFakeProUser puts the original cursorAuth/* values back so Cursor
// can authenticate normally again after our proxy is stopped. Reads the
// JSON sidecar produced by InjectFakeProUser and removes it on success.
// A nil entry in the sidecar means the key didn't exist before — we delete
// it rather than recreate empty.
func RestoreFakeProUser(backupPath string) error {
	if backupPath == "" {
		return nil
	}
	data, err := os.ReadFile(backupPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var backup map[string]*string
	if err := json.Unmarshal(data, &backup); err != nil {
		return fmt.Errorf("parse backup: %w", err)
	}
	dbPath := stateDBPath()
	if dbPath == "" {
		return fmt.Errorf("could not resolve Cursor state.vscdb path")
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout=2000")
	if err != nil {
		return err
	}
	defer db.Close()
	for key, val := range backup {
		if val == nil {
			if _, err := db.Exec("DELETE FROM ItemTable WHERE key = ?", key); err != nil {
				return fmt.Errorf("restore delete %s: %w", key, err)
			}
			continue
		}
		if _, err := db.Exec(
			"INSERT INTO ItemTable(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			key, *val,
		); err != nil {
			return fmt.Errorf("restore upsert %s: %w", key, err)
		}
	}
	_ = os.Remove(backupPath)
	return nil
}

// ForceModelSelection rewrites Cursor's locally cached model preferences so
// that every chat / agent / cmd-K feature points at the supplied BYOK hex
// model ID. Without this Cursor's picker keeps showing whatever model name
// was cached from a previous session — even if our AvailableModels rewrite
// announces a new id, the picker treats the cached selection as the source
// of truth and renders nothing when it doesn't match.
//
// The structure mirrors what the closed-source working app leaves in SQLite
// after a successful chat session.
func ForceModelSelection(byokHexID string) error {
	if byokHexID == "" {
		return nil
	}
	dbPath := stateDBPath()
	if dbPath == "" {
		return fmt.Errorf("could not resolve Cursor state.vscdb path")
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil // Cursor not installed yet
	}
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout=2000")
	if err != nil {
		return err
	}
	defer db.Close()

	var raw string
	if err := db.QueryRow("SELECT value FROM ItemTable WHERE key = ?", reactiveStorageKey).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return fmt.Errorf("applicationUser JSON parse: %w", err)
	}

	rewriteAiSettings(d, byokHexID)
	rewriteFeatureModelConfigs(d, byokHexID)

	out, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = db.Exec("UPDATE ItemTable SET value = ? WHERE key = ?", string(out), reactiveStorageKey)
	return err
}

func rewriteAiSettings(d map[string]any, hexID string) {
	ai, _ := d["aiSettings"].(map[string]any)
	if ai == nil {
		ai = map[string]any{}
		d["aiSettings"] = ai
	}

	mc, _ := ai["modelConfig"].(map[string]any)
	if mc == nil {
		mc = map[string]any{}
		ai["modelConfig"] = mc
	}

	// Features the working app pins to the BYOK model. background-composer is
	// deliberately left at "default" — that's how the working app does it.
	features := []string{"cmd-k", "composer", "composer-ensemble", "plan-execution", "spec", "deep-search", "quick-agent"}
	for _, f := range features {
		entry := map[string]any{
			"modelName":      hexID,
			"maxMode":        false,
			"selectedModels": nil,
		}
		if f == "composer" {
			entry["selectedModels"] = []any{
				map[string]any{
					"modelId":    hexID,
					"parameters": []any{},
				},
			}
		}
		mc[f] = entry
	}
	if _, ok := mc["background-composer"]; !ok {
		mc["background-composer"] = map[string]any{
			"modelName":      "default",
			"maxMode":        true,
			"selectedModels": nil,
		}
	}

	ai["modelOverrideEnabled"] = []any{}
	ai["modelsWithNoDefaultSwitch"] = []any{hexID}
	ai["modelDefaultSwitchOnNewChat"] = false

	prev, _ := ai["previousModelBeforeDefault"].(map[string]any)
	if prev == nil {
		prev = map[string]any{}
		ai["previousModelBeforeDefault"] = prev
	}
	for _, f := range []string{"cmd-k", "composer", "composer-ensemble", "plan-execution", "spec", "deep-search", "quick-agent"} {
		prev[f] = hexID
	}
}

func rewriteFeatureModelConfigs(d map[string]any, hexID string) {
	fmc, _ := d["featureModelConfigs"].(map[string]any)
	if fmc == nil {
		fmc = map[string]any{}
		d["featureModelConfigs"] = fmc
	}
	hexList := []any{hexID}
	pinFull := func(name string, withBestOfN bool) {
		entry := map[string]any{
			"defaultModel":   hexID,
			"fallbackModels": hexList,
		}
		if withBestOfN {
			entry["bestOfNDefaultModels"] = hexList
		} else {
			entry["bestOfNDefaultModels"] = []any{}
		}
		fmc[name] = entry
	}
	pinDefaultOnly := func(name string) {
		fmc[name] = map[string]any{
			"defaultModel":         hexID,
			"fallbackModels":       []any{},
			"bestOfNDefaultModels": []any{},
		}
	}
	pinFull("composer", true)
	pinFull("cmdK", false)
	pinFull("backgroundComposer", true)
	pinFull("planExecution", false)
	pinDefaultOnly("spec")
	pinDefaultOnly("deepSearch")
	pinDefaultOnly("quickAgent")
}

func stateDBPath() string {
	switch runtime.GOOS {
	case "windows":
		root := os.Getenv("APPDATA")
		if root == "" {
			return ""
		}
		return filepath.Join(root, "Cursor", "User", "globalStorage", "state.vscdb")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	default:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
	}
}
