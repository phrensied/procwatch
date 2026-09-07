package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type config struct {
	exePath      string
	exeArgs      string
	watchDirs    string
	pollInterval time.Duration
	logFile      string
	noDefaults   bool
	maxDepth     int
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.exePath, "exe", "", "Путь к исполняемому файлу, за которым нужно следить (обязательно)")
	flag.StringVar(&c.exeArgs, "args", "", "Аргументы командной строки для запускаемой программы, через пробел")
	flag.StringVar(&c.watchDirs, "watch", "", "Список каталогов для отслеживания изменений файлов через запятую (дополнительно к стандартным)")
	flag.DurationVar(&c.pollInterval, "interval", 700*time.Millisecond, "Частота опроса процессов/файлов")
	flag.StringVar(&c.logFile, "log", "", "Путь к файлу отчёта (если не указан — только вывод в консоль)")
	flag.BoolVar(&c.noDefaults, "no-defaults", false, "Не добавлять стандартные каталоги (temp, appdata и т.д.) в отслеживание")
	flag.IntVar(&c.maxDepth, "depth", 6, "Максимальная глубина рекурсии при сканировании каталогов")
	flag.Parse()
	return c
}

type Logger struct {
	mu      sync.Mutex
	console *log.Logger
	file    *log.Logger
}

func newLogger(path string) (*Logger, func(), error) {
	l := &Logger{console: log.New(os.Stdout, "", 0)}
	closeFn := func() {}
	if path != "" {
		f, err := os.Create(path)
		if err != nil {
			return nil, nil, err
		}
		l.file = log.New(f, "", 0)
		closeFn = func() { f.Close() }
	}
	return l, closeFn, nil
}

func (l *Logger) Printf(kind, format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().Format("15:04:05.000")
	line := fmt.Sprintf("[%s] [%-8s] %s", ts, kind, fmt.Sprintf(format, args...))
	l.console.Println(line)
	if l.file != nil {
		l.file.Println(line)
	}
}

type procInfo struct {
	ppid int
	name string
}

func snapshotProcesses() (map[int]procInfo, error) {
	if runtime.GOOS == "windows" {
		return snapshotProcessesWindows()
	}
	return snapshotProcessesUnix()
}

func snapshotProcessesUnix() (map[int]procInfo, error) {
	result := make(map[int]procInfo)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := 0
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil {
			continue
		}
		statPath := filepath.Join("/proc", e.Name(), "stat")
		data, err := os.ReadFile(statPath)
		if err != nil {
			continue
		}
		s := string(data)
		open := strings.IndexByte(s, '(')
		close := strings.LastIndexByte(s, ')')
		if open < 0 || close < 0 || close < open {
			continue
		}
		name := s[open+1 : close]
		rest := strings.Fields(s[close+2:])
		if len(rest) < 2 {
			continue
		}
		ppid := 0
		fmt.Sscanf(rest[1], "%d", &ppid)
		result[pid] = procInfo{ppid: ppid, name: name}
	}
	return result, nil
}

func snapshotProcessesWindows() (map[int]procInfo, error) {
	result := make(map[int]procInfo)
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"Get-CimInstance Win32_Process | Select-Object ProcessId,ParentProcessId,Name | ForEach-Object { \"$($_.ProcessId),$($_.ParentProcessId),$($_.Name)\" }")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 3)
		if len(parts) != 3 {
			continue
		}
		var pid, ppid int
		fmt.Sscanf(parts[0], "%d", &pid)
		fmt.Sscanf(parts[1], "%d", &ppid)
		result[pid] = procInfo{ppid: ppid, name: parts[2]}
	}
	return result, nil
}

func watchProcesses(logger *Logger, rootPid int, interval time.Duration, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	known := make(map[int]bool)
	tracked := map[int]bool{rootPid: true}

	for {
		select {
		case <-stop:
			return
		default:
		}

		snap, err := snapshotProcesses()
		if err == nil {
			changed := true
			for changed {
				changed = false
				for pid, info := range snap {
					if tracked[pid] {
						continue
					}
					if tracked[info.ppid] {
						tracked[pid] = true
						changed = true
					}
				}
			}

			for pid := range tracked {
				if info, ok := snap[pid]; ok && !known[pid] {
					known[pid] = true
					if pid == rootPid {
						logger.Printf("PROCESS", "Запущен основной процесс PID=%d (%s)", pid, info.name)
					} else {
						logger.Printf("PROCESS", "Создан дочерний процесс PID=%d PPID=%d (%s)", pid, info.ppid, info.name)
					}
				}
			}
			for pid := range known {
				if _, ok := snap[pid]; !ok {
					delete(known, pid)
					logger.Printf("PROCESS", "Процесс PID=%d завершился", pid)
				}
			}
			if _, alive := snap[rootPid]; !alive {
				anyChildAlive := false
				for pid := range tracked {
					if pid == rootPid {
						continue
					}
					if _, ok := snap[pid]; ok {
						anyChildAlive = true
						break
					}
				}
				if !anyChildAlive {
					return
				}
			}
		}

		select {
		case <-stop:
			return
		case <-time.After(interval):
		}
	}
}

type fileState struct {
	size    int64
	modTime time.Time
}

func defaultWatchDirs(exeDir string) []string {
	dirs := []string{exeDir}
	if runtime.GOOS == "windows" {
		env := func(k string) string { return os.Getenv(k) }
		candidates := []string{
			env("TEMP"), env("TMP"), env("APPDATA"), env("LOCALAPPDATA"),
			env("ProgramData"), env("ProgramFiles"), env("ProgramFiles(x86)"),
		}
		if home := env("USERPROFILE"); home != "" {
			candidates = append(candidates, filepath.Join(home, "Desktop"))
			candidates = append(candidates, filepath.Join(home, "Start Menu"))
		}
		for _, c := range candidates {
			if c != "" {
				dirs = append(dirs, c)
			}
		}
	} else {
		dirs = append(dirs, os.TempDir())
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, home)
		}
	}
	return dedupe(dirs)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = filepath.Clean(s)
		if s == "" || seen[s] {
			continue
		}
		if _, err := os.Stat(s); err != nil {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func scanDirs(dirs []string, maxDepth int) map[string]fileState {
	result := make(map[string]fileState)
	for _, root := range dirs {
		walkLimited(root, root, maxDepth, result)
	}
	return result
}

func walkLimited(root, dir string, depthLeft int, out map[string]fileState) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		if e.IsDir() {
			if depthLeft > 0 {
				walkLimited(root, full, depthLeft-1, out)
			}
			continue
		}
		out[full] = fileState{size: info.Size(), modTime: info.ModTime()}
	}
}

func watchFiles(logger *Logger, dirs []string, maxDepth int, interval time.Duration, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)

	prev := scanDirs(dirs, maxDepth)

	for {
		select {
		case <-stop:
			return
		case <-time.After(interval):
		}

		cur := scanDirs(dirs, maxDepth)

		for path, st := range cur {
			if old, ok := prev[path]; !ok {
				logger.Printf("FILE", "Создан файл: %s (%d байт)", path, st.size)
			} else if old.size != st.size || !old.modTime.Equal(st.modTime) {
				logger.Printf("FILE", "Изменён файл: %s (%d -> %d байт)", path, old.size, st.size)
			}
		}
		for path := range prev {
			if _, ok := cur[path]; !ok {
				logger.Printf("FILE", "Удалён файл: %s", path)
			}
		}

		prev = cur

		select {
		case <-stop:
			return
		default:
		}
	}
}

func splitArgs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var args []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case r == ' ' && !inQuotes:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

func main() {
	cfg := parseFlags()

	if cfg.exePath == "" {
		fmt.Println("Использование: procwatch -exe <путь к программе> [-args \"...\"] [-watch \"dir1,dir2\"] [-log report.log]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	absExe, err := filepath.Abs(cfg.exePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось получить абсолютный путь:", err)
		os.Exit(1)
	}
	if _, err := os.Stat(absExe); err != nil {
		fmt.Fprintln(os.Stderr, "Файл не найден:", absExe)
		os.Exit(1)
	}

	logger, closeLog, err := newLogger(cfg.logFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось создать лог-файл:", err)
		os.Exit(1)
	}
	defer closeLog()

	watchDirs := []string{}
	if !cfg.noDefaults {
		watchDirs = append(watchDirs, defaultWatchDirs(filepath.Dir(absExe))...)
	} else {
		watchDirs = append(watchDirs, filepath.Dir(absExe))
	}
	if cfg.watchDirs != "" {
		for _, d := range strings.Split(cfg.watchDirs, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				watchDirs = append(watchDirs, d)
			}
		}
	}
	watchDirs = dedupe(watchDirs)

	logger.Printf("INFO", "Цель: %s", absExe)
	logger.Printf("INFO", "Аргументы: %q", cfg.exeArgs)
	logger.Printf("INFO", "Отслеживаемые каталоги: %s", strings.Join(watchDirs, "; "))
	logger.Printf("INFO", "Начинаю мониторинг...")

	cmd := exec.Command(absExe, splitArgs(cfg.exeArgs)...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "Не удалось запустить программу:", err)
		os.Exit(1)
	}
	rootPid := cmd.Process.Pid
	logger.Printf("INFO", "Программа запущена, PID=%d", rootPid)

	go pipeToLog(logger, "STDOUT", stdout)
	go pipeToLog(logger, "STDERR", stderr)

	stopProc := make(chan struct{})
	doneProc := make(chan struct{})
	go watchProcesses(logger, rootPid, cfg.pollInterval, stopProc, doneProc)

	stopFiles := make(chan struct{})
	doneFiles := make(chan struct{})
	go watchFiles(logger, watchDirs, cfg.maxDepth, cfg.pollInterval, stopFiles, doneFiles)

	waitErr := cmd.Wait()
	if waitErr != nil {
		logger.Printf("INFO", "Программа завершилась с ошибкой: %v", waitErr)
	} else {
		logger.Printf("INFO", "Программа завершилась успешно (код 0)")
	}

	logger.Printf("INFO", "Ожидание завершения дочерних процессов (5с) перед остановкой слежения...")
	time.Sleep(5 * time.Second)

	close(stopProc)
	close(stopFiles)
	<-doneProc
	<-doneFiles

	logger.Printf("INFO", "Мониторинг завершён.")
}

func pipeToLog(logger *Logger, kind string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		logger.Printf(kind, "%s", scanner.Text())
	}
}
