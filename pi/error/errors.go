package errors

import (
	"context"
	stderrors "errors"
	"fmt"
)

type CodeError struct {
	code  int
	msg   string
	cause error
}

// New 声明一个稳定码错误值，对应 duserr.NewBizError(code int, msg string)。
func New(code int, msg string) *CodeError {
	return &CodeError{code: code, msg: msg}
}

func (e *CodeError) Code() int       { return e.code }
func (e *CodeError) Message() string { return e.msg }

func (e *CodeError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%d|%s: %v", e.code, e.msg, e.cause)
	}
	return fmt.Sprintf("%d|%s", e.code, e.msg)
}

func (e *CodeError) Unwrap() error { return e.cause }

// Wrap 复用稳定码与文案，把原始原因挂到错误链上。
func (e *CodeError) Wrap(err error) error {
	if err == nil {
		return nil
	}
	return &CodeError{code: e.code, msg: e.msg, cause: err}
}

// Params 填充文案中的占位符（fmt.Sprintf 语义），码与原因链不变。
func (e *CodeError) Params(args ...any) *CodeError {
	return &CodeError{code: e.code, msg: fmt.Sprintf(e.msg, args...), cause: e.cause}
}

// Is 按码匹配（对应 user 的按码 Is），其余目标透传原因链。
func (e *CodeError) Is(target error) bool {
	if coded, ok := stderrors.AsType[*CodeError](target); ok {
		return coded.code == e.code
	}
	return stderrors.Is(e.Unwrap(), target)
}

// Match 判断 err 链是否归属本码——判断码的首选写法：
func (e *CodeError) Match(err error) bool {
	return CodeOf(err) == e.code
}

// coded 由领域错误类型实现（如 loopdetect.Error、governor limitError），
// 声明其归属的稳定码，即被 CodeOf 识别。
type coded interface {
	error
	Code() int
}

var (
	// ErrUnknown 通用
	ErrUnknown          = New(10000, "未知错误")
	ErrInitialization   = New(10001, "初始化失败")
	ErrRequestInvalid   = New(10002, "请求无效")
	ErrWorkspaceInvalid = New(10003, "工作区无效")

	// ErrAIGeneration AI 生成
	ErrAIGeneration      = New(20000, "AI 生成失败")
	ErrAITransient       = New(20001, "AI 调用暂时性失败")
	ErrAIRateLimited     = New(20002, "AI 调用被限频")
	ErrAIContextOverflow = New(20003, "上下文超出模型窗口")
	ErrAIUnauthorized    = New(20004, "AI 平台鉴权失败")
	ErrAIQuotaExceeded   = New(20005, "AI 平台配额不足")
	ErrAIInvalidRequest  = New(20006, "AI 请求参数无效")
	ErrAIWorkDirIsNil    = New(20007, "pi: workdir is required")
	ErrAIAPIKeyRequired  = New(20008, "apiKey 不能为空")
	ErrAIModelRequired   = New(20009, "model 不能为空")
	ErrAIBaseURLRequired = New(20010, "baseURL 不能为空")

	// ErrToolRuntime 工具
	ErrToolRuntime          = New(30000, "工具执行失败")
	ErrToolInvalidArguments = New(30001, "工具参数无效")
	ErrToolResourceNotFound = New(30002, "工具资源不存在")
	ErrToolPermissionDenied = New(30003, "工具权限不足")
	ErrToolEditNoMatch      = New(30004, "未找到编辑目标")
	ErrToolEditNotUnique    = New(30005, "编辑目标不唯一")
	ErrToolTimeout          = New(30006, "工具执行超时")
	ErrToolPanic            = New(30007, "工具执行异常")

	// 工具注册与定义
	ErrToolDefinitionInvalid = New(30008, "工具定义无效")
	ErrToolAlreadyRegistered = New(30009, "工具已注册")
	ErrToolRegistryFrozen    = New(30010, "工具注册表已冻结")

	// ErrCanceled 取消与超时
	ErrCanceled         = New(40000, "已取消")
	ErrDeadlineExceeded = New(40001, "已超时")

	// ErrRunLimitExceeded 运行控制
	ErrRunLimitExceeded = New(50000, "运行预算超限")
	ErrRunLoopDetected  = New(50001, "检测到循环调用")

	// ErrClosed 生命周期
	ErrClosed = New(60000, "Agent 已关闭")

	// Skill 诊断（70000–70099）：仅进入 Skill Diagnostic 与日志，
	ErrSkillFileUnreadable          = New(70001, "SKILL.md 无法检查或读取")
	ErrSkillFileTooLarge            = New(70002, "SKILL.md 超过 256 KiB")
	ErrSkillFrontMatterMissing      = New(70003, "SKILL.md 缺少首行 FrontMatter")
	ErrSkillFrontMatterUnclosed     = New(70004, "SKILL.md FrontMatter 未闭合")
	ErrSkillFrontMatterInvalid      = New(70005, "SKILL.md FrontMatter YAML 无效")
	ErrSkillFrontMatterControlChars = New(70006, "SKILL.md FrontMatter 包含非法控制字符")
	ErrSkillNameMissing             = New(70007, "Skill name 不能为空")
	ErrSkillNameInvalid             = New(70008, "Skill name 格式无效")
	ErrSkillDescriptionMissing      = New(70009, "Skill description 不能为空")
	ErrSkillDescriptionTooLong      = New(70010, "Skill description 超过 1024 个字符")
	ErrSkillBodyEmpty               = New(70011, "Skill Body 不能为空")
	ErrSkillBinaryContent           = New(70012, "SKILL.md 包含 NUL 字节")
	ErrSkillNotUTF8                 = New(70013, "SKILL.md 不是有效的 UTF-8 文本")
	ErrSkillDuplicateName           = New(70014, "同一来源存在重复 Skill name")
	ErrSkillShadowed                = New(70015, "Skill 被更高优先级来源覆盖")
	ErrSkillModelInvocationDisabled = New(70016, "Skill 已禁止模型调用")

	ErrWorkDirRequired      = New(10004, "workDir 不能为空")
	ErrWorkDirUnopenable    = New(10005, "工作区目录无法打开")
	ErrAgentsFileMissing    = New(10006, "AGENTS.md 不存在")
	ErrAgentsFileUnreadable = New(10007, "AGENTS.md 读取失败")
	ErrAgentsFileNotRegular = New(10008, "AGENTS.md 不是普通文件")
	ErrAgentsFileTooLarge   = New(10009, "AGENTS.md 超过 1 MiB")
	ErrAgentsFileNotUTF8    = New(10010, "AGENTS.md 不是有效的 UTF-8 文本")
	ErrAgentsFileEmpty      = New(10011, "AGENTS.md 不能为空")

	// ErrInternal 内部
	ErrInternal = New(90000, "内部错误")
)

// CodeOf 沿错误链提取稳定码；nil 或未分类错误返回 0。
func CodeOf(err error) int {
	if err == nil {
		return 0
	}

	if classified, ok := stderrors.AsType[coded](err); ok {
		return classified.Code()
	}

	switch {
	case stderrors.Is(err, context.Canceled):
		return ErrCanceled.Code()
	case stderrors.Is(err, context.DeadlineExceeded):
		return ErrDeadlineExceeded.Code()
	}
	return 0
}
