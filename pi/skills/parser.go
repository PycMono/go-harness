package skills

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"

	pierrors "github.com/PycMono/go-harness/pi/error"
	"gopkg.in/yaml.v3"
)

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type parsedSkill struct {
	Name                   string
	Description            string
	OS                     []string
	RequiredBins           []string
	RequiredEnv            []string
	DisableModelInvocation bool
}

type skillFrontMatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

type skillParseError struct {
	Code    string
	Message string
}

func newSkillParseError(code string, message string) *skillParseError {
	return &skillParseError{
		Code:    code,
		Message: message,
	}
}

// Error 返回适合暴露给诊断信息的 Skill 解析错误描述。
func (e *skillParseError) Error() string {
	return e.Message
}

// 技能解析
func parseToSkill(content []byte) (*parsedSkill, error) {
	if bytes.IndexByte(content, 0) >= 0 {
		return nil, pierrors.ErrSkillBinaryContent
	}
	if !utf8.Valid(content) {
		return nil, pierrors.ErrSkillNotUTF8
	}

	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return nil, pierrors.ErrSkillFrontMatterMissing
	}

	closing := -1
	for index := 1; index < len(lines); index++ {
		if lines[index] == "---" {
			closing = index
			break
		}
	}
	if closing == -1 {
		return nil, pierrors.ErrSkillFrontMatterUnclosed
	}

	var metadata skillFrontMatter
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:closing], "\n")), &metadata); err != nil {
		return nil, pierrors.ErrSkillFrontMatterInvalid
	}

	name := strings.TrimSpace(metadata.Name)
	if name == "" {
		return nil, pierrors.ErrSkillNameMissing
	}
	if utf8.RuneCountInString(name) > 64 || !skillNamePattern.MatchString(name) {
		return nil, pierrors.ErrSkillNameInvalid
	}

	description := strings.TrimSpace(metadata.Description)
	if description == "" {
		return nil, pierrors.ErrSkillDescriptionMissing
	}
	if utf8.RuneCountInString(description) > 1024 {
		return nil, pierrors.ErrSkillDescriptionTooLong
	}
	if !isValidXMLText(description) {
		return nil, pierrors.ErrSkillFrontMatterControlChars
	}

	body := strings.TrimSpace(strings.Join(lines[closing+1:], "\n"))
	if body == "" {
		return nil, pierrors.ErrSkillBodyEmpty
	}

	return &parsedSkill{
		Name:                   name,
		Description:            description,
		DisableModelInvocation: metadata.DisableModelInvocation,
	}, nil
}
