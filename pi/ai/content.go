package tools

import (
	"fmt"
	"net/url"
	"strings"
)

// ContentType 表示消息内容块的类型。
type ContentType string

// ContentTypeText 表示纯文本内容块。
const ContentTypeText ContentType = "text"

// ContentTypeImage 表示 URL 图像内容块。
const ContentTypeImage ContentType = "image"

// ContentBlocks is an ordered collection of message content blocks.
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
	// URL 是图像的可访问地址；调用方必须保证推理服务商可访问且生命周期足够长。
	URL string `json:"url"`
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

// Validate 校验图像内容的 URL：必须是带 host 的 http/https 地址。
func (image ImageContent) Validate() error {
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

// Validate 逐个校验内容块，不涉及消息角色。角色相关的限制（例如只允许
// user 消息携带图片）属于消息层策略，由上层在调用本函数后自行约束。
func (blocks ContentBlocks) Validate() error {
	for _, block := range blocks {
		if err := block.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Clone deep-copies the backing slice and Image pointers.
func (blocks ContentBlocks) Clone() ContentBlocks {
	if blocks == nil {
		return nil
	}
	cloned := make(ContentBlocks, len(blocks))
	for index, block := range blocks {
		cloned[index] = block
		if block.Image != nil {
			image := *block.Image
			cloned[index].Image = &image
		}
	}
	return cloned
}

// WithImagePlaceholders returns a copy where image blocks are replaced by
// redacted text placeholders.
func (blocks ContentBlocks) WithImagePlaceholders() ContentBlocks {
	result := make(ContentBlocks, 0, len(blocks))
	for _, block := range blocks {
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

// Text concatenates text blocks in order and rejects non-text content.
func (blocks ContentBlocks) Text() (string, error) {
	var builder strings.Builder
	for _, block := range blocks {
		if block.Type != ContentTypeText {
			return "", fmt.Errorf("unsupported content type %q", block.Type)
		}
		builder.WriteString(block.Text)
	}
	return builder.String(), nil
}
