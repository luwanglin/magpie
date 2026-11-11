package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/jsonc"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
)

// AgySettingsFlag reports whether the agy named (empty: the one on PATH)
// takes --settings. agy 1.4.8 dropped it, and the only file it reads is
// settings.json. One that can't be asked is taken to have the flag, which
// is what every agy had until then, so a machine with no agy keeps the
// command it had. Tests replace it: they never ask the agy installed here.
var AgySettingsFlag = probeAgySettingsFlag

var agyFlagCache struct {
	sync.Mutex
	key string
	ok  bool
}

// probeAgySettingsFlag reads agy's --help once per binary. The answer is
// kept until that file changes, so a page that asks for the launch command
// again doesn't start agy each time.
func probeAgySettingsFlag(bin string) bool {
	if bin == "" {
		bin = "agy"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return true
	}
	st, err := os.Stat(path)
	if err != nil {
		return true
	}
	key := path + "\x00" + st.ModTime().UTC().Format(time.RFC3339Nano) + "\x00" + strconv.FormatInt(st.Size(), 10)
	agyFlagCache.Lock()
	if agyFlagCache.key == key {
		ok := agyFlagCache.ok
		agyFlagCache.Unlock()
		return ok
	}
	agyFlagCache.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, path, "--help").CombinedOutput()
	ok := bytes.Contains(out, []byte("--settings"))
	if ctx.Err() == nil {
		agyFlagCache.Lock()
		agyFlagCache.key = key
		agyFlagCache.ok = ok
		agyFlagCache.Unlock()
	}
	return ok
}

// agyHeldLaunch is the command copied from the row when agy has no
// --settings: magpie agy run puts magpie's settings into settings.json
// for that process and puts the user's file back after. On Windows the
// name magpie is the console program a shell waits for, not the GUI app.
func agyHeldLaunch(gw, key, model string, powershell bool) string {
	if powershell {
		return `$env:GEMINI_API_KEY="` + key + `"; $env:GOOGLE_GEMINI_BASE_URL="` + gw + `"; magpie agy run --model '` + strings.ReplaceAll(model, "'", "''") + `'`
	}
	return "GEMINI_API_KEY=" + key + " GOOGLE_GEMINI_BASE_URL=" + gw + " magpie agy run --model '" + strings.ReplaceAll(model, "'", `'\''`) + "'"
}

// agyHeldPowerShell is the function for a shell that would otherwise pass
// --settings itself. It runs magpie <id> run, which holds settings.json,
// through magpie.com when that sits beside the app: the app is a GUI
// program and a shell doesn't wait for it.
func agyHeldPowerShell(id, bin, prog string) string {
	q := "'" + strings.ReplaceAll(prog, "'", "''") + "'"
	return "function " + bin + " {\n" +
		"    $real = Get-Command " + bin + " -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1\n" +
		"    if (-not $real) { Write-Error '" + bin + " is not installed'; return }\n" +
		"    $exe = " + q + "\n" +
		"    $com = Join-Path (Split-Path -LiteralPath $exe) 'magpie.com'\n" +
		"    $run = $exe\n" +
		"    if (Test-Path -LiteralPath $com) { $run = $com }\n" +
		"    if (Test-Path -LiteralPath $run) { & $run " + id + " run @args; return }\n" +
		"    & $real @args\n" +
		"}"
}

// agyAppliedPath is the settings.json a distro copies into place when its
// agy has no --settings: the user's file with magpie's model, provider and
// custom models laid over it, written beside magpie-settings.json.
func agyAppliedPath(over string) string {
	return strings.TrimSuffix(over, ".json") + ".applied.json"
}

// agyWriteApplied writes that file from the user's settings and magpie's.
// A user file that isn't JSON is left out of it; the distro then copies
// magpie's file alone for the session.
func agyWriteApplied(user, over string) error {
	ub, err := os.ReadFile(user)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	ob, err := os.ReadFile(over)
	if err != nil {
		return err
	}
	merged, err := agyMergeSettings(ub, ob)
	if err != nil {
		merged = append(append([]byte(nil), bytes.TrimSpace(ob)...), '\n')
	}
	return agyWriteAtomic(agyAppliedPath(over), merged)
}

// agyHold puts magpie's settings over the user's for this process and
// returns what puts the user's file back. The copy of the user's file and
// the pids of the magpies waiting live beside it, under one lock, so two
// runs share the overlaid file and a magpie that died is put back by the
// next instead of the overlaid file being taken for the user's.
func agyHold(user, over string) (func(), error) {
	if user == "" || over == "" {
		return nil, errors.New("agy: no settings to hold")
	}
	unlock, _ := filelock.Lock(user+".lock", filelock.Wait)
	defer unlock()
	if err := agyHoldAcquire(user, over); err != nil {
		return nil, err
	}
	return func() { agyHoldRelease(user) }, nil
}

func agyHoldAcquire(user, over string) error {
	dir := user + ".magpie-holders"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	agySweepHolders(dir)
	empty, err := agyHoldersEmpty(dir)
	if err != nil {
		return err
	}
	if empty {
		if err := agyRestoreUser(user); err != nil {
			return err
		}
		if err := agyBackupUser(user); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), nil, 0o644); err != nil {
		return err
	}
	return agyOverlayFile(user, over)
}

func agyHoldRelease(user string) {
	unlock, _ := filelock.Lock(user+".lock", filelock.Wait)
	defer unlock()
	dir := user + ".magpie-holders"
	_ = os.Remove(filepath.Join(dir, strconv.Itoa(os.Getpid())))
	agySweepHolders(dir)
	empty, err := agyHoldersEmpty(dir)
	if err != nil || !empty {
		return
	}
	_ = agyRestoreUser(user)
	_ = os.Remove(dir)
}

func agyBackupUser(user string) error {
	absent := user + ".magpie-absent"
	backup := user + ".magpie-user"
	_, err := os.Stat(user)
	if os.IsNotExist(err) {
		return os.WriteFile(absent, nil, 0o644)
	}
	if err != nil {
		return err
	}
	b, err := os.ReadFile(user)
	if err != nil {
		return err
	}
	_ = os.Remove(absent)
	return os.WriteFile(backup, b, 0o644)
}

func agyRestoreUser(user string) error {
	absent := user + ".magpie-absent"
	backup := user + ".magpie-user"
	if _, err := os.Stat(absent); err == nil {
		_ = os.Remove(user)
		return os.Remove(absent)
	}
	b, err := os.ReadFile(backup)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := agyWriteAtomic(user, b); err != nil {
		return err
	}
	return os.Remove(backup)
}

func agySweepHolders(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !agyProcLive(pid) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func agyHoldersEmpty(dir string) (bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(ents) == 0, nil
}

func agyProcLive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	_, err := proc.Started(pid)
	return err == nil || errors.Is(err, proc.ErrNoStart)
}

func agyOverlayFile(user, over string) error {
	ub, err := os.ReadFile(user)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	ob, err := os.ReadFile(over)
	if err != nil {
		return err
	}
	merged, err := agyMergeSettings(ub, ob)
	if err != nil {
		return err
	}
	return agyWriteAtomic(user, merged)
}

// agyMergeSettings is the user's settings with model, modelProvider and
// customModelsConfig taken from magpie's file. Everything else of the
// user's, trustedWorkspaces among it, stays.
func agyMergeSettings(user, over []byte) ([]byte, error) {
	u, err := agyJSONMap(user)
	if err != nil {
		return nil, err
	}
	o, err := agyJSONMap(over)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"model", "modelProvider", "customModelsConfig"} {
		if v, ok := o[k]; ok {
			u[k] = v
		}
	}
	out, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func agyJSONMap(b []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	in := append([]byte(nil), b...)
	var m map[string]any
	if err := json.Unmarshal(jsonc.ToJSONInPlace(in), &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func agyWriteAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".magpie-settings-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, 0o644)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			os.Remove(tmp)
			return err
		}
	}
	return nil
}

// The distro's function has no magpie. It asks agy --help: a help that
// lists --settings is started with the flag, as before; one whose first
// words are a usage line and that doesn't list it gets magpie's settings
// for that process, from the applied file when writeOver left one, and
// the user's file back after. A help that says neither keeps the flag, so
// a stand-in that only echoes its arguments still does.
const agyHoldAcquireSh = `
pid=$1
over=$2
f="$HOME/.gemini/antigravity-cli/settings.json"
d="$f.magpie-holders"
b="$f.magpie-user"
lock="$f.magpie-hold.lock"
n=0
while ! mkdir "$lock" 2>/dev/null; do
  n=$((n+1))
  if [ "$n" -ge 300 ]; then
    echo "agy: magpie's settings are held" >&2
    exit 1
  fi
  sleep 0.05
done
mkdir -p "$d" || { rmdir "$lock"; exit 1; }
for p in "$d"/*; do
  [ -e "$p" ] || continue
  h=${p##*/}
  kill -0 "$h" 2>/dev/null || rm -f "$p"
done
left=0
for p in "$d"/*; do
  if [ -e "$p" ]; then
    left=1
    break
  fi
done
if [ "$left" -eq 0 ]; then
  if [ -f "$f.magpie-absent" ]; then
    rm -f "$f" "$f.magpie-absent"
  elif [ -f "$b" ]; then
    mv "$b" "$f"
  fi
  if [ -f "$f" ]; then
    cp "$f" "$b"
  else
    : > "$f.magpie-absent"
  fi
fi
: > "$d/$pid"
applied="${over%.json}.applied.json"
if [ -f "$applied" ]; then
  cp "$applied" "$f"
else
  cp "$over" "$f"
fi
rmdir "$lock"
`

const agyHoldReleaseSh = `
pid=$1
f="$HOME/.gemini/antigravity-cli/settings.json"
d="$f.magpie-holders"
b="$f.magpie-user"
lock="$f.magpie-hold.lock"
n=0
while ! mkdir "$lock" 2>/dev/null; do
  n=$((n+1))
  if [ "$n" -ge 300 ]; then
    exit 1
  fi
  sleep 0.05
done
rm -f "$d/$pid"
for p in "$d"/*; do
  [ -e "$p" ] || continue
  h=${p##*/}
  kill -0 "$h" 2>/dev/null || rm -f "$p"
done
left=0
for p in "$d"/*; do
  if [ -e "$p" ]; then
    left=1
    break
  fi
done
if [ "$left" -eq 0 ]; then
  if [ -f "$f.magpie-absent" ]; then
    rm -f "$f" "$f.magpie-absent"
  elif [ -f "$b" ]; then
    mv "$b" "$f"
  fi
  rmdir "$d" 2>/dev/null || true
fi
rmdir "$lock"
`

func agyDistroPosixSettings(settings string) string {
	script := posixQuote(strings.TrimSpace(agyHoldAcquireSh))
	return "  _agy_hold=0\n" +
		"  _agy_h=$(command agy --help 2>&1 || true)\n" +
		"  case \"$_agy_h\" in\n" +
		"    *--settings*)\n" +
		"      set -- --settings " + settings + ` "$@"` + "\n" +
		"      ;;\n" +
		"    *Usage\\ of*)\n" +
		"      _agy_hold=1\n" +
		"      sh -c " + script + " sh \"$$\" " + settings + "\n" +
		"      ;;\n" +
		"    *)\n" +
		"      set -- --settings " + settings + ` "$@"` + "\n" +
		"      ;;\n" +
		"  esac\n"
}

func agyDistroPosixRelease() string {
	script := posixQuote(strings.TrimSpace(agyHoldReleaseSh))
	return "  _agy_st=$?\n" +
		"  if [ \"${_agy_hold:-0}\" = 1 ]; then sh -c " + script + " sh \"$$\"; fi\n" +
		"  unset _agy_h _agy_hold\n" +
		"  return $_agy_st\n"
}

func agyDistroFishSettings(settings string) string {
	script := fishQuote(strings.TrimSpace(agyHoldAcquireSh))
	return "    set -l _agy_lines (command agy --help 2>&1)\n" +
		"    set -l _agy_h \"$_agy_lines\"\n" +
		"    set -l _agy_hold 0\n" +
		"    switch \"$_agy_h\"\n" +
		"        case '*--settings*'\n" +
		"            set argv --settings " + settings + " $argv\n" +
		"        case '*Usage of*'\n" +
		"            set _agy_hold 1\n" +
		"            sh -c " + script + " sh $fish_pid " + settings + "\n" +
		"        case '*'\n" +
		"            set argv --settings " + settings + " $argv\n" +
		"    end\n"
}

func agyDistroFishRelease() string {
	script := fishQuote(strings.TrimSpace(agyHoldReleaseSh))
	return "    set -l _agy_st $status\n" +
		"    if test \"$_agy_hold\" = 1\n" +
		"        sh -c " + script + " sh $fish_pid\n" +
		"    end\n" +
		"    return $_agy_st\n"
}
