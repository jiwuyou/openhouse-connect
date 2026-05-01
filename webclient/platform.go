package webclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/webclient/internal/store"
)

type replyContext struct {
	Project string
	Session string
}

type platform struct {
	s       *Server
	project string
}

func newPlatform(s *Server, project string) *platform {
	return &platform{s: s, project: strings.TrimSpace(project)}
}

func (p *platform) Name() string { return "webclient" }

func (p *platform) Start(handler core.MessageHandler) error {
	project := p.project
	if err := store.ValidateSegment("project", project); err != nil {
		return err
	}
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	p.s.projectHandlers[project] = handler
	if p.s.projectPlatforms == nil {
		p.s.projectPlatforms = make(map[string]*platform)
	}
	p.s.projectPlatforms[project] = p
	return nil
}

func (p *platform) Stop() error {
	project := p.project
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	delete(p.s.projectHandlers, project)
	if p.s.projectPlatforms != nil {
		delete(p.s.projectPlatforms, project)
	}
	return nil
}

func (p *platform) Reply(ctx context.Context, replyCtx any, content string) error {
	return p.Send(ctx, replyCtx, content)
}

func (p *platform) Send(ctx context.Context, replyCtx any, content string) error {
	rc, err := parseReplyCtx(replyCtx)
	if err != nil {
		return err
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	msg := store.Message{
		Role:    store.RoleAssistant,
		Content: content,
	}
	stored, err := p.s.store.AppendMessage(rc.Project, rc.Session, msg)
	if err != nil {
		return err
	}
	p.s.events.Publish(rc.Project, rc.Session, stored)
	return nil
}

func (p *platform) SendImage(ctx context.Context, replyCtx any, img core.ImageAttachment) error {
	rc, err := parseReplyCtx(replyCtx)
	if err != nil {
		return err
	}
	meta, att, err := p.s.storeSaveImage(img)
	if err != nil {
		return err
	}
	msg := store.Message{
		Role: store.RoleAssistant,
		Attachments: []store.Attachment{
			{
				ID:       meta.ID,
				Kind:     "image",
				FileName: meta.FileName,
				MimeType: meta.MimeType,
				Size:     meta.Size,
				URL:      att.URL,
			},
		},
	}
	stored, err := p.s.store.AppendMessage(rc.Project, rc.Session, msg)
	if err != nil {
		return err
	}
	p.s.events.Publish(rc.Project, rc.Session, stored)
	return nil
}

func (p *platform) SendFile(ctx context.Context, replyCtx any, file core.FileAttachment) error {
	rc, err := parseReplyCtx(replyCtx)
	if err != nil {
		return err
	}
	meta, att, err := p.s.storeSaveFile(file)
	if err != nil {
		return err
	}
	msg := store.Message{
		Role: store.RoleAssistant,
		Attachments: []store.Attachment{
			{
				ID:       meta.ID,
				Kind:     "file",
				FileName: meta.FileName,
				MimeType: meta.MimeType,
				Size:     meta.Size,
				URL:      att.URL,
			},
		},
	}
	stored, err := p.s.store.AppendMessage(rc.Project, rc.Session, msg)
	if err != nil {
		return err
	}
	p.s.events.Publish(rc.Project, rc.Session, stored)
	return nil
}

func (p *platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	project, session, err := parseSessionKey(sessionKey)
	if err != nil {
		return nil, err
	}
	return replyContext{Project: project, Session: session}, nil
}

func parseReplyCtx(v any) (replyContext, error) {
	switch t := v.(type) {
	case replyContext:
		return t, nil
	case *replyContext:
		if t == nil {
			return replyContext{}, fmt.Errorf("webclient: replyCtx is nil")
		}
		return *t, nil
	default:
		return replyContext{}, fmt.Errorf("webclient: invalid replyCtx type %T", v)
	}
}

func sessionKey(project, session string) string {
	return "webclient:" + project + ":" + session
}

func parseSessionKey(key string) (project string, session string, err error) {
	key = strings.TrimSpace(key)
	const prefix = "webclient:"
	if !strings.HasPrefix(key, prefix) {
		return "", "", fmt.Errorf("webclient: unsupported session key %q", key)
	}
	rest := strings.TrimPrefix(key, prefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("webclient: invalid session key %q", key)
	}
	project = parts[0]
	session = parts[1]
	if err := store.ValidateSegment("project", project); err != nil {
		return "", "", err
	}
	if err := store.ValidateSegment("session", session); err != nil {
		return "", "", err
	}
	return project, session, nil
}

var _ core.Platform = (*platform)(nil)
var _ core.ImageSender = (*platform)(nil)
var _ core.FileSender = (*platform)(nil)
var _ core.ReplyContextReconstructor = (*platform)(nil)

