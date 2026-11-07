package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Версия встроенного yt-dlp. Меняйте вместе с tools/yt-dlp.exe.
const ytDlpVersion = "2026.10.26"

//go:embed tools/yt-dlp.exe
var embeddedYtDlp []byte

var stdin = bufio.NewReader(os.Stdin)

type videoInfo struct {
	ID       string            `json:"id"`
	Type     string            `json:"_type"`
	Title    string            `json:"title"`
	Uploader string            `json:"uploader"`
	Duration float64           `json:"duration"`
	Formats  []formatInfo      `json:"formats"`
	Entries  []json.RawMessage `json:"entries"`
}

type formatInfo struct {
	FormatID       string  `json:"format_id"`
	Ext            string  `json:"ext"`
	VCodec         string  `json:"vcodec"`
	ACodec         string  `json:"acodec"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	FPS            float64 `json:"fps"`
	FormatNote     string  `json:"format_note"`
	Filesize       int64   `json:"filesize"`
	FilesizeApprox int64   `json:"filesize_approx"`
	TBR            float64 `json:"tbr"`
	ABR            float64 `json:"abr"`
	Protocol       string  `json:"protocol"`
	Language       string  `json:"language"`
}

type qualityOption struct {
	height    int
	quality   string
	video     *formatInfo
	audio     *formatInfo // nil, если формат уже содержит звук
	size      int64
	label     string
	container string
}

func main() {
	setupConsole()

	err := run()
	exitCode := 0
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\nЗагрузка отменена.")
			exitCode = 130
		} else {
			fmt.Fprintln(os.Stderr, "\nОшибка:", err)
			exitCode = 1
		}
	}

	pause()
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Println("==============================================")
	fmt.Println("       YouTube Downloader (терминал)")
	fmt.Println("==============================================")

	rawURL := ""
	if len(os.Args) > 1 {
		rawURL = strings.TrimSpace(os.Args[1])
	}
	if rawURL == "" {
		fmt.Print("\nВставьте ссылку на видео YouTube: ")
		line, err := stdin.ReadString('\n')
		if err != nil && len(line) == 0 {
			return fmt.Errorf("не удалось прочитать ссылку: %w", err)
		}
		rawURL = strings.TrimSpace(line)
	}
	if rawURL == "" {
		return errors.New("ссылка не указана")
	}

	videoURL, err := normalizeURL(rawURL)
	if err != nil {
		return err
	}

	ytDlpPath, err := ensureYtDlp()
	if err != nil {
		return err
	}
	ffmpegPath := findFFmpeg()

	var info *videoInfo
	err = withSpinner("Получаю информацию о видео", func() error {
		v, err := fetchVideoInfo(ctx, ytDlpPath, videoURL)
		if err != nil {
			return err
		}
		info = v
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("не удалось получить данные видео: %w", err)
	}

	printVideoInfo(info)

	options := buildQualityOptions(info, ffmpegPath != "")
	if len(options) == 0 {
		if ffmpegPath == "" {
			return errors.New("не найдено ни одного готового формата со звуком, а ffmpeg не установлен.\n" +
				"Установите ffmpeg (https://ffmpeg.org) и добавьте его в PATH, затем запустите программу снова")
		}
		return errors.New("не удалось найти доступные форматы для скачивания")
	}
	if ffmpegPath == "" {
		fmt.Println("\nВнимание: ffmpeg не найден. Для скачивания нужно объединить")
		fmt.Println("видео и звук, поэтому доступны только готовые форматы со звуком.")
		fmt.Println("Установите ffmpeg и добавьте его в PATH.")
	}

	choice, err := askQuality(options)
	if err != nil {
		return err
	}
	opt := options[choice]

	fmt.Printf("\nВыбрано: %s\n", opt.label)

	workDir, err := os.MkdirTemp(".", ".ytdl-*")
	if err != nil {
		return fmt.Errorf("не удалось создать временную папку: %w", err)
	}
	defer os.RemoveAll(workDir)

	fmt.Printf("\nСкачивание: %s\n", opt.label)
	outBase := filepath.Join(workDir, "output")
	if err := downloadWithYtDlp(ctx, ytDlpPath, ffmpegPath, videoURL, opt, outBase); err != nil {
		return err
	}

	src, err := findOutputFile(workDir)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	finalPath := uniqueOutputPath(cwd, info.Title, opt, filepath.Ext(src))
	if err := moveFile(src, finalPath); err != nil {
		return fmt.Errorf("не удалось сохранить файл: %w", err)
	}

	fmt.Printf("\nГотово! Файл сохранён:\n%s\n", finalPath)
	return nil
}

// ---------- yt-dlp ----------

func ensureYtDlp() (string, error) {
	// Локальный yt-dlp рядом с программой имеет приоритет — его можно обновить
	// без пересборки.
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "yt-dlp.exe")
		if isFile(candidate) {
			return candidate, nil
		}
	}
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "yt-dlp.exe")
		if isFile(candidate) {
			return candidate, nil
		}
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" {
		cacheDir = os.TempDir()
	}
	dir := filepath.Join(cacheDir, "YouTubeDownloader")
	target := filepath.Join(dir, fmt.Sprintf("yt-dlp-%s.exe", ytDlpVersion))

	if fi, err := os.Stat(target); err == nil && fi.Size() == int64(len(embeddedYtDlp)) {
		return target, nil
	}

	if err := os.MkdirAll(dir, 0o755); err == nil {
		if err := writeFileAtomic(target, embeddedYtDlp); err == nil {
			return target, nil
		}
	}

	// Запасной вариант — временная папка.
	target = filepath.Join(os.TempDir(), fmt.Sprintf("yt-dlp-%s.exe", ytDlpVersion))
	if err := writeFileAtomic(target, embeddedYtDlp); err == nil {
		return target, nil
	}

	if path, err := exec.LookPath("yt-dlp"); err == nil {
		return path, nil
	}
	return "", errors.New("не удалось подготовить yt-dlp. Установите yt-dlp и добавьте его в PATH")
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".yt-dlp-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmpName, 0o755)
	return os.Rename(tmpName, path)
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func fetchVideoInfo(ctx context.Context, ytDlp, videoURL string) (*videoInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(cctx, ytDlp,
		"--no-warnings", "--no-playlist", "-J", videoURL)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, errors.New("YouTube слишком долго не отвечает")
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(lastLines(msg, 6))
	}

	data := bytes.TrimSpace(stdout.Bytes())
	if i := bytes.IndexByte(data, '{'); i > 0 {
		data = data[i:]
	}

	var info videoInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("не удалось разобрать ответ YouTube: %w", err)
	}
	if info.Type == "playlist" || (info.Title != "" && len(info.Formats) == 0 && len(info.Entries) > 0) {
		return nil, errors.New("это плейлист. Укажите ссылку на конкретное видео")
	}
	if info.Title == "" {
		return nil, errors.New("видео недоступно (возможно, это закрытый ролик или требуется вход)")
	}
	if len(info.Formats) == 0 {
		return nil, errors.New("YouTube не вернул ни одного формата")
	}
	return &info, nil
}

func downloadWithYtDlp(ctx context.Context, ytDlp, ffmpeg, videoURL string, opt qualityOption, outBase string) error {
	format := opt.video.FormatID
	if format == "" {
		format = fmt.Sprintf("bv*[height=%d]", opt.height)
	}
	if opt.audio != nil {
		if opt.audio.FormatID != "" {
			format += "+" + opt.audio.FormatID
		} else {
			format += "+ba"
		}
	} else if !hasAudio(opt.video) {
		format += "+ba"
	}

	err := runDownload(ctx, ytDlp, ffmpeg, videoURL, format, opt.container, outBase)
	if err == nil {
		return nil
	}
	if isFormatUnavailable(err) {
		fmt.Println("\n  Выбранный формат недоступен, пробую подобрать автоматически...")
		fallback := fmt.Sprintf("bv*[height=%d]+ba/b[height=%d]", opt.height, opt.height)
		return runDownload(ctx, ytDlp, ffmpeg, videoURL, fallback, opt.container, outBase)
	}
	return err
}

func runDownload(ctx context.Context, ytDlp, ffmpeg, videoURL, format, container, outBase string) error {
	args := []string{
		"--no-warnings", "--no-playlist",
		"--retries", "10",
		"--socket-timeout", "30",
		"-f", format,
		"-o", outBase + ".%(ext)s",
	}
	if container != "" {
		args = append(args, "--merge-output-format", strings.TrimPrefix(container, "."))
	}
	if ffmpeg != "" {
		args = append(args, "--ffmpeg-location", filepath.Dir(ffmpeg))
	}
	args = append(args, videoURL)
	return runYtDlp(ctx, ytDlp, args...)
}

func runYtDlp(ctx context.Context, ytDlp string, args ...string) error {
	cmd := exec.CommandContext(ctx, ytDlp, args...)
	var tail tailBuffer
	tail.max = 16 * 1024

	cmd.Stdout = io.MultiWriter(os.Stdout, &tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, &tail)

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := strings.TrimSpace(tail.String())
		if msg == "" {
			return err
		}
		return errors.New(lastLines(msg, 8))
	}
	return nil
}

func isFormatUnavailable(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "requested format is not available") ||
		strings.Contains(msg, "format is not available")
}

func findOutputFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}

	var best string
	var bestSize int64 = -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "output.") ||
			strings.HasSuffix(name, ".part") ||
			strings.HasSuffix(name, ".ytdl") {
			continue
		}
		fi, err := entry.Info()
		if err != nil {
			continue
		}
		if fi.Size() > bestSize {
			best = filepath.Join(dir, name)
			bestSize = fi.Size()
		}
	}
	if best == "" {
		return "", errors.New("yt-dlp не создал выходной файл")
	}
	return best, nil
}

type tailBuffer struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.data = append(t.data, p...)
	if t.max <= 0 {
		t.max = 8 * 1024
	}
	if len(t.data) > t.max {
		copy(t.data, t.data[len(t.data)-t.max:])
		t.data = t.data[:t.max]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			out = append([]string{line}, out...)
		}
	}
	return strings.Join(out, "\n")
}

// ---------- Форматы ----------

func buildQualityOptions(info *videoInfo, haveFFmpeg bool) []qualityOption {
	videoByHeight := make(map[int]*formatInfo)
	muxedByHeight := make(map[int]*formatInfo)
	var audios []*formatInfo

	for i := range info.Formats {
		f := &info.Formats[i]
		if !isHTTPProtocol(f.Protocol) || strings.EqualFold(f.Ext, "mhtml") {
			continue
		}
		if strings.EqualFold(f.FormatNote, "storyboard") {
			continue
		}

		hasVideo := f.VCodec != "" && f.VCodec != "none"
		hasAudioTrack := f.ACodec != "" && f.ACodec != "none"
		if !hasVideo {
			if hasAudioTrack {
				audios = append(audios, f)
			}
			continue
		}
		if f.Height <= 0 {
			continue
		}

		if hasAudioTrack {
			if cur, ok := muxedByHeight[f.Height]; !ok || betterVideo(f, cur) {
				muxedByHeight[f.Height] = f
			}
			continue
		}
		if cur, ok := videoByHeight[f.Height]; !ok || betterVideo(f, cur) {
			videoByHeight[f.Height] = f
		}
	}

	heightSet := make(map[int]bool)
	for h := range videoByHeight {
		heightSet[h] = true
	}
	for h := range muxedByHeight {
		heightSet[h] = true
	}

	heights := make([]int, 0, len(heightSet))
	for h := range heightSet {
		heights = append(heights, h)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(heights)))

	options := make([]qualityOption, 0, len(heights))
	for _, h := range heights {
		vf := videoByHeight[h]
		mf := muxedByHeight[h]

		if haveFFmpeg && vf != nil {
			if af := pickAudio(audios, vf); af != nil {
				options = append(options, newOption(vf, af, info.Duration))
				continue
			}
		}
		if mf != nil {
			options = append(options, newOption(mf, nil, info.Duration))
		}
	}
	return options
}

func isHTTPProtocol(protocol string) bool {
	return strings.HasPrefix(strings.ToLower(protocol), "http")
}

func betterVideo(a, b *formatInfo) bool {
	ra, rb := videoCodecRank(a), videoCodecRank(b)
	if ra != rb {
		return ra > rb
	}
	if a.FPS != b.FPS {
		return a.FPS > b.FPS
	}
	return a.TBR > b.TBR
}

func videoCodecRank(f *formatInfo) int {
	v := strings.ToLower(f.VCodec)
	switch {
	case strings.HasPrefix(v, "avc1"), strings.HasPrefix(v, "h264"):
		return 4
	case strings.HasPrefix(v, "av01"):
		return 3
	case strings.HasPrefix(v, "vp9"), strings.HasPrefix(v, "vp09"):
		return 2
	case strings.HasPrefix(v, "vp8"):
		return 1
	}
	return 0
}

func pickAudio(audios []*formatInfo, vf *formatInfo) *formatInfo {
	wantExt := "m4a"
	if strings.EqualFold(vf.Ext, "webm") {
		wantExt = "webm"
	}

	var best *formatInfo
	bestScore := -1
	for _, f := range audios {
		score := 0
		if strings.EqualFold(f.Ext, wantExt) {
			score += 100
		}
		note := strings.ToLower(f.FormatNote)
		if strings.Contains(note, "original") {
			score += 30
		}
		if strings.Contains(note, "default") {
			score += 15
		}
		if score > bestScore || (score == bestScore && audioBitrate(f) > audioBitrate(best)) {
			best = f
			bestScore = score
		}
	}
	return best
}

func audioBitrate(f *formatInfo) float64 {
	if f == nil {
		return 0
	}
	if f.ABR > 0 {
		return f.ABR
	}
	return f.TBR
}

func hasAudio(f *formatInfo) bool {
	return f != nil && f.ACodec != "" && f.ACodec != "none"
}

func newOption(vf, af *formatInfo, duration float64) qualityOption {
	opt := qualityOption{
		height:    vf.Height,
		quality:   videoQualityLabel(vf),
		video:     vf,
		audio:     af,
		container: outputContainer(vf, af),
	}
	opt.size = formatSize(vf, duration)
	if af != nil {
		opt.size += formatSize(af, duration)
	}

	resolution := opt.quality
	if vf.Width > 0 && vf.Height > 0 {
		resolution += fmt.Sprintf(" (%dx%d)", vf.Width, vf.Height)
	}
	if vf.FPS > 30 {
		resolution += fmt.Sprintf(" %dfps", int(vf.FPS+0.5))
	}

	codecs := videoCodecName(vf.VCodec)
	if af != nil {
		codecs += " + " + audioCodecName(af.ACodec)
	} else if hasAudio(vf) {
		codecs += " (со звуком)"
	}

	parts := []string{resolution, strings.TrimPrefix(opt.container, ".") + "/" + codecs}
	if opt.size > 0 {
		parts = append(parts, "~"+humanSize(opt.size))
	}
	opt.label = strings.Join(parts, " · ")
	return opt
}

func videoQualityLabel(f *formatInfo) string {
	if f.FormatNote != "" {
		if match := resolutionRe.FindString(strings.ToLower(f.FormatNote)); match != "" {
			return match
		}
	}
	if f.Height > 0 {
		return fmt.Sprintf("%dp", f.Height)
	}
	return "video"
}

var resolutionRe = regexp.MustCompile(`\d{3,4}p\d*`)

func videoCodecName(codec string) string {
	v := strings.ToLower(codec)
	switch {
	case strings.HasPrefix(v, "avc1"), strings.HasPrefix(v, "h264"):
		return "H.264"
	case strings.HasPrefix(v, "av01"):
		return "AV1"
	case strings.HasPrefix(v, "vp9"), strings.HasPrefix(v, "vp09"):
		return "VP9"
	case strings.HasPrefix(v, "vp8"):
		return "VP8"
	case strings.HasPrefix(v, "hev1"), strings.HasPrefix(v, "hvc1"):
		return "H.265"
	}
	if v == "" || v == "none" {
		return "видео"
	}
	return v
}

func audioCodecName(codec string) string {
	a := strings.ToLower(codec)
	switch {
	case strings.HasPrefix(a, "mp4a"):
		return "AAC"
	case strings.HasPrefix(a, "opus"):
		return "Opus"
	case strings.HasPrefix(a, "ac-3"):
		return "AC-3"
	case strings.HasPrefix(a, "ec-3"):
		return "E-AC-3"
	}
	if a == "" || a == "none" {
		return "аудио"
	}
	return a
}

func outputContainer(vf, af *formatInfo) string {
	ve := strings.ToLower(vf.Ext)
	if af == nil {
		if ve == "mp4" || ve == "webm" {
			return "." + ve
		}
		return ".mkv"
	}

	ae := strings.ToLower(af.Ext)
	if ve == "mp4" && ae == "m4a" {
		return ".mp4"
	}
	if ve == "webm" && ae == "webm" {
		return ".webm"
	}
	return ".mkv"
}

func formatSize(f *formatInfo, duration float64) int64 {
	if f == nil {
		return 0
	}
	if f.Filesize > 0 {
		return f.Filesize
	}
	if f.FilesizeApprox > 0 {
		return f.FilesizeApprox
	}
	bitrate := f.TBR
	if bitrate <= 0 {
		bitrate = f.ABR
	}
	if bitrate > 0 && duration > 0 {
		// tbr в yt-dlp указан в килобитах в секунду.
		return int64(bitrate * 1000 / 8 * duration)
	}
	return 0
}

// ---------- Ввод и вывод ----------

func printVideoInfo(v *videoInfo) {
	title := v.Title
	if utf8.RuneCountInString(title) > 100 {
		title = string([]rune(title)[:100]) + "..."
	}
	fmt.Printf("\nНазвание:     %s\n", title)
	if v.Uploader != "" {
		fmt.Printf("Автор:        %s\n", v.Uploader)
	}
	if v.Duration > 0 {
		fmt.Printf("Длительность: %s\n", formatDuration(time.Duration(v.Duration*float64(time.Second))))
	}
}

func askQuality(options []qualityOption) (int, error) {
	fmt.Println("\nДоступные качества:")
	fmt.Println("----------------------------------------------")
	for i, opt := range options {
		mark := ""
		if i == 0 {
			mark = "  <- лучшее"
		}
		fmt.Printf("  %2d) %s%s\n", i+1, opt.label, mark)
	}
	fmt.Println("----------------------------------------------")

	for {
		fmt.Print("Выберите качество [1]: ")
		line, err := stdin.ReadString('\n')
		if err != nil && len(line) == 0 {
			return 0, fmt.Errorf("не удалось прочитать ответ: %w", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(line)
		if err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		fmt.Printf("Введите число от 1 до %d.\n", len(options))
	}
}

// ---------- Вспомогательные функции ----------

func withSpinner(msg string, fn func() error) error {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		frames := []string{"|", "/", "-", "\\"}
		i := 0
		for {
			select {
			case <-done:
				return
			default:
				fmt.Printf("\r%s %s", msg, frames[i%len(frames)])
				i++
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	err := fn()
	close(done)
	wg.Wait()
	fmt.Printf("\r%s\r", strings.Repeat(" ", utf8.RuneCountInString(msg)+3))
	return err
}

var videoIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

func normalizeURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'<>`)

	if videoIDRe.MatchString(s) {
		return "https://www.youtube.com/watch?v=" + s, nil
	}

	if !strings.HasPrefix(strings.ToLower(s), "http://") && !strings.HasPrefix(strings.ToLower(s), "https://") {
		s = "https://" + s
	}

	low := strings.ToLower(s)
	if !strings.Contains(low, "youtube.com") && !strings.Contains(low, "youtu.be") {
		return "", errors.New("это не похоже на ссылку YouTube. Пример: https://www.youtube.com/watch?v=XXXXXXXXXXX")
	}
	return s, nil
}

func findFFmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "ffmpeg.exe")
		if isFile(candidate) {
			return candidate
		}
	}
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "ffmpeg.exe")
		if isFile(candidate) {
			return candidate
		}
	}
	return ""
}

func uniqueOutputPath(dir, title string, opt qualityOption, ext string) string {
	base := sanitizeFilename(title)
	if strings.TrimSpace(base) == "" {
		base = "video"
	}
	base = fmt.Sprintf("%s [%s]", base, opt.quality)

	if ext == "" {
		ext = opt.container
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}

	path := filepath.Join(dir, base+ext)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	for i := 1; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

var invalidFilenameChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = invalidFilenameChars.ReplaceAllString(name, "-")
	name = strings.Join(strings.Fields(name), " ")
	name = strings.TrimRight(name, " .")

	runes := []rune(name)
	if len(runes) > 120 {
		name = string(runes[:120])
		name = strings.TrimRight(name, " .")
	}

	upper := strings.ToUpper(name)
	base := upper
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	reserved := map[string]bool{
		"CON": true, "PRN": true, "AUX": true, "NUL": true,
		"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
		"COM6": true, "COM7": true, "COM8": true, "COM9": true,
		"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
		"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	}
	if reserved[base] {
		name = "_" + name
	}
	return name
}

func humanSize(n int64) string {
	if n < 0 {
		n = 0
	}
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.2f ГБ", float64(n)/gb)
	case n >= mb:
		return fmt.Sprintf("%.1f МБ", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.1f КБ", float64(n)/kb)
	default:
		return fmt.Sprintf("%d Б", n)
	}
}

func formatDuration(d time.Duration) string {
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// pause не даёт окну закрыться сразу при запуске двойным щелчком.
func pause() {
	if len(os.Args) > 1 || !isTerminalInput() {
		return
	}
	fmt.Print("\nНажмите Enter, чтобы выйти...")
	_, _ = stdin.ReadString('\n')
}
