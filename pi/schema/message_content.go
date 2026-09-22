package schema

import (
	"fmt"
	"net/url"
	"strings"
)

// 本文件承载内容块值类型：ContentType、ContentBlock、ImageContent 与 ContentBlocks，
// 以及各自的构造、校验与文本/脱敏投影。这里不 import 任何模型 SDK；把内容块翻译成
// 协议格式的投影（含图片 URL 的选用）在 message_convert.go。

// ContentType 表示消息内容块的类型。
type ContentType string

// ContentTypeText 表示纯文本内容块。
const ContentTypeText ContentType = "text"

// ContentTypeImage 表示 URL 图像内容块。
const ContentTypeImage ContentType = "image"

// ContentBlocks is an ordered collection of message content b.
type ContentBlocks []ContentBlock

// ContentBlock 表示消息中的一个内容块。联合类型取值受 Validate 约束：
// text 块不得携带 Image，image 块只携带 Image 不携带 Text。
type ContentBlock struct {
	// Type 表示内容块的类型。
	Type ContentType `json:"type"`
	// Text 保存文本内容。
	Text string `json:"text,omitempty"`
	// Image 保存 URL 图像内容；仅 Type 为 ContentTypeImage 时非空。
	Image *ImageContent `json:"image,omitempty"`
}

// ImageContent 表示一个 URL 图像内容。
type ImageContent struct {
	// Data 是 base64 编码的图像数据，和 MIMEType 一起对齐 Pi 的内联图片协议。
	Data string `json:"data,omitempty"`
	// MIMEType 是 Data 对应的媒体类型，例如 image/png。
	MIMEType string `json:"mime_type,omitempty"`
	// URL 保留现有远程图片输入；新代码优先使用 Data + MIMEType。
	URL string `json:"url,omitempty"`
}

// TextBlock 创建一个纯文本内容块。
func TextBlock(text string) ContentBlock {
	return ContentBlock{Type: ContentTypeText, Text: text}
}

// ImageBlock 创建一个 URL 图像内容块。
func ImageBlock(imageURL string) ContentBlock {
	return ContentBlock{Type: ContentTypeImage, Image: &ImageContent{URL: imageURL}}
}

// Validate 校验内容块的联合类型取值：text 块不得携带 Image，image 块必须
// 只携带合法 URL 的 Image，未知类型报错。校验集中在入口边界复用本函数，
// 不散落到使用方。
func (block ContentBlock) Validate() error {
	switch block.Type {
	case ContentTypeText:
		if block.Image != nil {
			return fmt.Errorf("text block must not carry an image")
		}
	case ContentTypeImage:
		if block.Text != "" {
			return fmt.Errorf("image block must not carry text")
		}
		if block.Image == nil {
			return fmt.Errorf("image block requires image content")
		}
		if err := block.Image.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported content type %q", block.Type)
	}
	return nil
}

// Validate 校验图像内容：必须是带 host 的 http/https URL，或同时提供 base64 数据和 MIME 类型。
func (image ImageContent) Validate() error {
	if image.Data != "" {
		if image.MIMEType == "" || !strings.HasPrefix(image.MIMEType, "image/") {
			return fmt.Errorf("inline image requires an image MIME type")
		}
		return nil
	}
	if image.URL == "" {
		return fmt.Errorf("image requires url or inline data")
	}
	parsed, err := url.Parse(image.URL)
	if err != nil {
		return fmt.Errorf("image url %q: %w", image.URL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("image url %q must use http or https", image.URL)
	}
	if parsed.Host == "" {
		return fmt.Errorf("image url %q requires a host", image.URL)
	}
	return nil
}

// Validate 校验内容块集合里的每个块及其联合类型取值。角色的图片规则不在这里
// 按角色分支：角色已由消息类型确定，由各具体类型的 Validate 各自附加。
func (b ContentBlocks) Validate() error {
	for _, block := range b {
		if err := block.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Clone deep-copies the backing slice and Image pointers.
func (b ContentBlocks) Clone() ContentBlocks {
	if b == nil {
		return nil
	}
	cloned := make(ContentBlocks, len(b))
	for index, block := range b {
		cloned[index] = block
		if block.Image != nil {
			image := *block.Image
			cloned[index].Image = &image
		}
	}
	return cloned
}

// WithImagePlaceholders returns a copy where image b are replaced by
// redacted text placeholders.
func (b ContentBlocks) WithImagePlaceholders() ContentBlocks {
	result := make(ContentBlocks, 0, len(b))
	for _, block := range b {
		if block.Type == ContentTypeImage && block.Image != nil {
			result = append(result, TextBlock(ImagePlaceholderText(block.Image.URL)))
			continue
		}
		result = append(result, block)
	}
	return result
}

// ImagePlaceholderText 生成图像块的脱敏占位文本：只保留 scheme、host 与
// path，剥离查询参数与片段，避免签名、临时 Token 泄漏到模型上下文。降级
// 占位与压缩摘要投影共用本函数。
func ImagePlaceholderText(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "[图片]"
	}
	brief := parsed.Scheme + "://" + parsed.Host + parsed.Path
	if parsed.Path == "" || parsed.Path == "/" {
		brief = parsed.Scheme + "://" + parsed.Host
	}
	return "[图片: " + brief + "]"
}

// Text concatenates text b in order and rejects non-text content.
func (b ContentBlocks) Text() (string, error) {
	var builder strings.Builder
	for _, block := range b {
		if block.Type != ContentTypeText {
			return "", fmt.Errorf("unsupported content type %q", block.Type)
		}
		builder.WriteString(block.Text)
	}
	return builder.String(), nil
}
