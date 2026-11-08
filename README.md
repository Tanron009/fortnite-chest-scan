# chestscan

Counts how many chests you opened in your Fortnite replays (`.replay` files), from a folder you choose.
Shows chests per match, your total, and your average per match.

**Fully vibecoded (written with Claude).** If that's a problem for you, don't use it.
Accuracy is not fully verified: it was tested on one replay from Fortnite build 43. Use `--debug` to sanity check it
against a match where you know your real chest count.

**I recommend copying your replay files somewhere else first (for example a folder on your desktop) and pointing the tool at that copy, so it can't accidentally touch any game files.** The tool only reads replay files, but a copy is the safest way.

## Use the ready-made exe
1. Unzip `chestscan-exe.zip`.
2. Double-click `chestscan.exe` and pick the folder with your replays (or drag the folder onto the exe).
   Command line: `chestscan.exe "D:\my replays" --debug --csv results.csv`

Options: `--debug` (show who opened the most chests in each match), `--csv file.csv` (save results),
`--name SomeName` (count that in-game name instead of the player who recorded the replay).

## Don't trust the exe? Build it yourself
1. Install Go from https://go.dev/dl/ (the exe was built with Go 1.24.7).
2. Unzip this source folder, open a terminal in it (PowerShell or cmd).
3. Run: `go build -trimpath -ldflags "-s -w" -o chestscan.exe .`
4. Run your own `chestscan.exe`.

Check the exe I shipped: `Get-FileHash chestscan.exe -Algorithm SHA256` (PowerShell) should match `SHA256.txt`.
Your own build may give a different hash if your Go version or platform differs; the program behaves the same.

## What it does / doesn't do
- Reads replay files only. No network code: the code uses only Go's standard library and never imports `net/http`
  or any other networking package (search the source to confirm).
- Writes nothing unless you pass `--csv`.
- The only program it starts is PowerShell, to show the folder picker (see `pickFolderWindows` in `main.go`).
- Counting: a chest counts when its "already searched" flag flips and the player who searched it is the player who
  recorded the replay (or `--name`). Ammo boxes, food boxes and loot cannons are not counted as chests.

## Files
- `main.go` command line, folder picker, results
- `analyze.go` chest counting
- `stream.go`, `bits.go`, `names.go`, `util.go` replay network stream parsing
- `container.go` replay file layout, AES decryption
- `oodle.go` Oodle (Mermaid) decompressor, a Go port of the Mermaid decoder from powzix/ooz

## License
GPLv3. `oodle.go` is derived from powzix/ooz (GPLv3), so the whole program is GPLv3.
