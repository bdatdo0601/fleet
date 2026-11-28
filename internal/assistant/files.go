package assistant

import (
	"cmp"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func resolveJailedPath(root, cleanRel, escapeMsg string) (string, error) {
	rootClean := filepath.Clean(root)
	rootReal, err := filepath.EvalSymlinks(rootClean)
	if err != nil {
		rootReal = rootClean
	}
	full := filepath.Join(rootClean, cleanRel)
	cur := full
	for {
		if resolved, rErr := filepath.EvalSymlinks(cur); rErr == nil {
			relCheck, relErr := filepath.Rel(rootReal, resolved)
			if relErr != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
				return "", newErr(escapeMsg, http.StatusForbidden)
			}
			return full, nil
		}
		if fi, lErr := os.Lstat(cur); lErr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", newErr(escapeMsg, http.StatusForbidden)
		}
		if cur == rootClean {
			return full, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", newErr(escapeMsg, http.StatusForbidden)
		}
		cur = parent
	}
}

func isSafeInlineImageMIME(m string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.Split(m, ";")[0]))
	switch base {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

// IsSafeInlinePreviewMIME reports whether mimeType is safe to serve inline (images excluding SVG, plus PDF).
func IsSafeInlinePreviewMIME(m string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.Split(m, ";")[0]))
	return isSafeInlineImageMIME(base) || base == "application/pdf"
}

// SanitizeFileName normalizes an uploaded or sandbox filename to a safe single path segment.
func SanitizeFileName(name string) string {
	norm := strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	base := filepath.Base(norm)
	if base == "" || base == "." || base == "/" || base == ".." {
		return "file.bin"
	}
	var b strings.Builder
	for _, r := range base {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	clean := strings.TrimLeft(b.String(), ". _")
	if clean == "" {
		return "file.bin"
	}
	if len(clean) > 120 {
		ext := filepath.Ext(clean)
		if len(ext) > 0 && len(ext) < 20 {
			clean = clean[:120-len(ext)] + ext
		} else {
			clean = clean[:120]
		}
	}
	return clean
}

var extraMIMEByExt = map[string]string{
	".aac":  "audio/aac",
	".csv":  "text/csv; charset=utf-8",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".flac": "audio/flac",
	".json": "application/json; charset=utf-8",
	".m4a":  "audio/mp4",
	".md":   "text/markdown; charset=utf-8",
	".mp3":  "audio/mpeg",
	".ogg":  "audio/ogg",
	".opus": "audio/opus",
	".pdf":  "application/pdf",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".wav":  "audio/wav",
	".webm": "audio/webm",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}

func detectFileMIME(name, headerMIME string, sample []byte) string {
	ext := strings.ToLower(filepath.Ext(name))
	if m, ok := extraMIMEByExt[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	if cleanHdr := strings.TrimSpace(strings.Split(headerMIME, ";")[0]); cleanHdr != "" && cleanHdr != "application/octet-stream" {
		return cleanHdr
	}
	if len(sample) > 0 {
		return http.DetectContentType(sample)
	}
	return "application/octet-stream"
}

func fileWidgetEntry(f AssistantFile) map[string]any {
	return map[string]any{
		"id":         f.ID,
		"name":       f.Name,
		"mime_type":  f.MimeType,
		"size_bytes": f.SizeBytes,
		"url":        "/v1/assistant/files/" + f.ID,
		"is_image":   isSafeInlineImageMIME(f.MimeType),
		"direction":  f.Direction,
	}
}

func formatByteSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func (e *Engine) maxSandboxFileBytes() int64 {
	mb := e.Config().Sandbox.MaxFileMB
	if mb <= 0 {
		mb = DefaultSandboxMaxFileMB
	}
	return int64(mb) * 1024 * 1024
}

func (e *Engine) stageFileToSandboxIn(channelID, srcPath, fileName string) error {
	if e.sandbox == nil {
		return nil
	}
	workDir, err := e.sandbox.WorkDir(channelID)
	if err != nil {
		return err
	}
	dst := filepath.Join(workDir, "in", fileName)
	return copyFileLimited(srcPath, dst, e.maxSandboxFileBytes(), 0o644)
}

func copyFileLimited(src, dst string, maxBytes int64, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	n, cpErr := io.Copy(out, io.LimitReader(in, maxBytes+1))
	closeErr := out.Close()
	if cpErr != nil {
		_ = os.Remove(dst)
		return cpErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	if maxBytes > 0 && n > maxBytes {
		_ = os.Remove(dst)
		return newErr(fmt.Sprintf("file exceeds %d MB limit", maxBytes/(1024*1024)), http.StatusRequestEntityTooLarge)
	}
	return nil
}

// SaveUploadedFile persists an uploaded channel file, enforces MaxFileMB, and stages a copy into /work/in/.
func (e *Engine) SaveUploadedFile(channelID, origName, headerMIME string, r io.Reader) (AssistantFile, error) {
	channelID = cmp.Or(strings.TrimSpace(channelID), "general")
	if _, err := e.store.GetChannel(channelID); err != nil {
		return AssistantFile{}, err
	}
	safeName := SanitizeFileName(origName)
	id := randomHexID("file")
	chDir := filepath.Join(e.filesDir, SanitizeFileName(channelID))
	if err := os.MkdirAll(chDir, 0o700); err != nil {
		return AssistantFile{}, err
	}
	dstPath := filepath.Join(chDir, id+"_"+safeName)
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return AssistantFile{}, err
	}
	maxBytes := e.maxSandboxFileBytes()
	n, cpErr := io.Copy(out, io.LimitReader(r, maxBytes+1))
	closeErr := out.Close()
	if cpErr != nil {
		_ = os.Remove(dstPath)
		return AssistantFile{}, cpErr
	}
	if closeErr != nil {
		_ = os.Remove(dstPath)
		return AssistantFile{}, closeErr
	}
	if n > maxBytes {
		_ = os.Remove(dstPath)
		return AssistantFile{}, newErr(fmt.Sprintf("file %q exceeds %d MB limit", safeName, e.Config().Sandbox.MaxFileMB), http.StatusRequestEntityTooLarge)
	}
	sample := readHeadSample(dstPath, 512)
	mimeType := detectFileMIME(safeName, headerMIME, sample)
	rec, err := e.store.CreateFile(AssistantFile{
		ID:          id,
		ChannelID:   channelID,
		Direction:   "upload",
		Name:        safeName,
		MimeType:    mimeType,
		SizeBytes:   n,
		StoragePath: dstPath,
	})
	if err != nil {
		_ = os.Remove(dstPath)
		return AssistantFile{}, err
	}
	if sErr := e.stageFileToSandboxIn(channelID, dstPath, safeName); sErr != nil {
		return AssistantFile{}, sErr
	}
	return rec, nil
}

func readHeadSample(path string, maxBytes int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, maxBytes)
	n, _ := io.ReadFull(f, buf)
	return buf[:n]
}

// OpenStoredFile validates storage path containment and opens a stored AssistantFile for download/preview.
func (e *Engine) OpenStoredFile(id string) (AssistantFile, *os.File, error) {
	f, err := e.store.GetFile(id)
	if err != nil {
		return AssistantFile{}, nil, err
	}
	rootReal, err := filepath.EvalSymlinks(e.filesDir)
	if err != nil {
		rootReal = filepath.Clean(e.filesDir)
	}
	fileReal, err := filepath.EvalSymlinks(f.StoragePath)
	if err != nil {
		if os.IsNotExist(err) {
			return AssistantFile{}, nil, newErr("stored file missing on disk", http.StatusNotFound)
		}
		return AssistantFile{}, nil, err
	}
	rel, err := filepath.Rel(rootReal, fileReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return AssistantFile{}, nil, newErr("invalid stored file path", http.StatusForbidden)
	}
	fh, err := os.Open(fileReal)
	if err != nil {
		return AssistantFile{}, nil, err
	}
	return *f, fh, nil
}

// PublishSandboxFile copies a sandbox file into persistent storage and builds a downloadable files card widget.
func (e *Engine) PublishSandboxFile(channelID, sandboxRelPath, title string) (AssistantFile, WidgetPayload, error) {
	if e.sandbox == nil {
		return AssistantFile{}, WidgetPayload{}, newErr("sandbox unavailable", http.StatusServiceUnavailable)
	}
	channelID = cmp.Or(strings.TrimSpace(channelID), "general")
	absSrc, _, err := e.sandbox.ResolveWorkPath(channelID, sandboxRelPath)
	if err != nil {
		return AssistantFile{}, WidgetPayload{}, err
	}
	info, err := os.Stat(absSrc)
	if err != nil {
		if os.IsNotExist(err) {
			return AssistantFile{}, WidgetPayload{}, newErr(fmt.Sprintf("sandbox file %q not found", sandboxRelPath), http.StatusNotFound)
		}
		return AssistantFile{}, WidgetPayload{}, err
	}
	if info.IsDir() {
		return AssistantFile{}, WidgetPayload{}, newErr("cannot publish a directory", http.StatusBadRequest)
	}
	maxBytes := e.maxSandboxFileBytes()
	if info.Size() > maxBytes {
		return AssistantFile{}, WidgetPayload{}, newErr(fmt.Sprintf("file %q exceeds %d MB limit", filepath.Base(absSrc), e.Config().Sandbox.MaxFileMB), http.StatusRequestEntityTooLarge)
	}
	safeName := SanitizeFileName(filepath.Base(absSrc))
	id := randomHexID("file")
	chDir := filepath.Join(e.filesDir, SanitizeFileName(channelID))
	if err := os.MkdirAll(chDir, 0o700); err != nil {
		return AssistantFile{}, WidgetPayload{}, err
	}
	dstPath := filepath.Join(chDir, id+"_"+safeName)
	if err := copyFileLimited(absSrc, dstPath, maxBytes, 0o600); err != nil {
		return AssistantFile{}, WidgetPayload{}, err
	}
	sample := readHeadSample(dstPath, 512)
	mimeType := detectFileMIME(safeName, "", sample)
	rec, err := e.store.CreateFile(AssistantFile{
		ID:          id,
		ChannelID:   channelID,
		Direction:   "generated",
		Name:        safeName,
		MimeType:    mimeType,
		SizeBytes:   info.Size(),
		StoragePath: dstPath,
	})
	if err != nil {
		_ = os.Remove(dstPath)
		return AssistantFile{}, WidgetPayload{}, err
	}
	widget := WidgetPayload{
		Kind:  "files",
		Title: cmp.Or(strings.TrimSpace(title), "Generated File"),
		Data:  map[string]any{"files": []any{fileWidgetEntry(rec)}},
	}
	return rec, widget, nil
}
