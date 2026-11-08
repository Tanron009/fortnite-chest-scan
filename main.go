// chestscan: counts the chests you opened in Fortnite replay files.
// Reads .replay files from a folder you choose; nothing is uploaded, nothing is written unless you pass --csv.
// Licensed GPLv3 (the Oodle decoder in oodle.go is derived from powzix/ooz).
package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

func pickFolderWindows() string {
	script := `Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; $d.Description = 'Choose the folder with your Fortnite .replay files'; if ($d.ShowDialog() -eq 'OK') { $d.SelectedPath }`
	out, err := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func main() {
	setupVerify()
	name := flag.String("name", "", "count a specific in-game name instead of the player who recorded the replay")
	csvPath := flag.String("csv", "", "also write the per-match results to this CSV file")
	debug := flag.Bool("debug", false, "show the top chest openers of each match (to sanity check the counting)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: chestscan [options] [folder-or-.replay-file]\n\nWith no folder it asks you to pick one.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	interactive := flag.NArg() == 0
	defer func() {
		if interactive && runtime.GOOS == "windows" {
			fmt.Print("\nPress Enter to close...")
			bufio.NewReader(os.Stdin).ReadString('\n')
		}
	}()

	target := ""
	if flag.NArg() > 0 {
		target = flag.Arg(0)
	} else {
		if runtime.GOOS == "windows" {
			target = pickFolderWindows()
		}
		if target == "" {
			fmt.Print("Folder with your .replay files: ")
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			target = strings.Trim(strings.TrimSpace(line), `"`)
		}
	}
	if target == "" {
		fmt.Println("No folder chosen.")
		return
	}
	var files []string
	st, err := os.Stat(target)
	if err != nil {
		fmt.Println("Can't open:", err)
		return
	}
	if st.IsDir() {
		ents, _ := os.ReadDir(target)
		for _, e := range ents {
			if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".replay") {
				files = append(files, filepath.Join(target, e.Name()))
			}
		}
	} else {
		files = []string{target}
	}
	if len(files) == 0 {
		fmt.Println("No .replay files found in", target)
		return
	}
	sort.Strings(files)
	fmt.Printf("Reading %d replay file(s) from %s\n\n", len(files), target)

	type outcome struct {
		res *MatchResult
		err error
	}
	results := make([]outcome, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, min(4, runtime.NumCPU()))
	var mu sync.Mutex
	done := 0
	for i, f := range files {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := analyzeReplay(f, *name)
			results[i] = outcome{res, err}
			mu.Lock()
			done++
			fmt.Fprintf(os.Stderr, "\r  analysed %d/%d", done, len(files))
			mu.Unlock()
		}(i, f)
	}
	wg.Wait()
	fmt.Fprintln(os.Stderr)
	fmt.Println()

	counted, total, skipped := 0, 0, 0
	best, worst := -1, -1
	type row struct {
		file     string
		chests   int
		ammo     int
		length   int
		recorder string
	}
	var rows []row
	for i, o := range results {
		base := filepath.Base(files[i])
		if o.res == nil {
			skipped++
			fmt.Printf("  skipped  %s: %v\n", base, o.err)
			continue
		}
		if o.err != nil {
			fmt.Printf("  warning  %s: %v\n", base, o.err)
		}
		r := o.res
		counted++
		total += r.Chests
		if best < 0 || r.Chests > best {
			best = r.Chests
		}
		if worst < 0 || r.Chests < worst {
			worst = r.Chests
		}
		rows = append(rows, row{base, r.Chests, r.Ammo, r.LengthSec, r.Recorder})
		fmt.Printf("  %-52s chests %3d   (ammo boxes %2d, %2dm%02ds)\n", shorten(base, 52), r.Chests, r.Ammo, r.LengthSec/60, r.LengthSec%60)
		if *debug {
			fmt.Printf("      recorder: %q   top openers in this match: %s\n", r.Recorder, strings.Join(topPlayers(r.PerPlayer, 6), ", "))
		}
	}
	fmt.Println()
	if counted == 0 {
		fmt.Println("Nothing could be read.")
		return
	}
	fmt.Printf("Matches counted : %d", counted)
	if skipped > 0 {
		fmt.Printf("  (%d skipped)", skipped)
	}
	fmt.Println()
	fmt.Printf("Chests opened   : %d\n", total)
	fmt.Printf("Average / match : %.1f\n", float64(total)/float64(counted))
	fmt.Printf("Most / fewest   : %d / %d\n", best, worst)

	if *csvPath != "" {
		f, err := os.Create(*csvPath)
		if err == nil {
			w := csv.NewWriter(f)
			w.Write([]string{"file", "chests", "ammo_boxes", "length_seconds", "recorder_name"})
			for _, r := range rows {
				w.Write([]string{r.file, fmt.Sprint(r.chests), fmt.Sprint(r.ammo), fmt.Sprint(r.length), r.recorder})
			}
			w.Flush()
			f.Close()
			fmt.Println("Wrote", *csvPath)
		} else {
			fmt.Println("Could not write CSV:", err)
		}
	}
	_ = time.Now
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n+3:]
}
