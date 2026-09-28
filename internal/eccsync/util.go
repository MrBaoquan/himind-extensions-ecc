package eccsync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extractTarGz 解开 GitHub 源码压缩包。
//
// GitHub 的压缩包外面套了一层 `<repo>-<sha>/`，这里顺手剥掉，
// 让上层看到的就是仓库根目录。
func extractTarGz(data []byte, target string) error {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("源码压缩包不是合法的 gzip: %w", err)
	}
	defer func() { _ = reader.Close() }()
	return extractTar(tar.NewReader(reader), target, true)
}

// extractGitArchive 解开 `git archive` 生成的 tar。
//
// 与源码压缩包不同，git archive 的路径本来就是仓库根目录，
// 没有外层包装目录，所以不剥前缀。
func extractGitArchive(data []byte, target string) error {
	return extractTar(tar.NewReader(bytes.NewReader(data)), target, false)
}

// extractTar 把 tar 流落到 target；stripTop 为真时丢掉第一段路径。
func extractTar(archive *tar.Reader, target string, stripTop bool) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取源码压缩包失败: %w", err)
		}
		name := filepath.ToSlash(header.Name)
		relative := name
		if stripTop {
			slash := strings.Index(name, "/")
			if slash < 0 {
				continue
			}
			relative = name[slash+1:]
		}
		if relative == "" || strings.Contains(relative, "..") {
			continue
		}
		destination := filepath.Join(target, filepath.FromSlash(relative))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return err
			}
			file, err := os.Create(destination)
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, io.LimitReader(archive, 64<<20)); err != nil {
				_ = file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}
