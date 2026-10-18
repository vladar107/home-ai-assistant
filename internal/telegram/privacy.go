package telegram

import "context"

// The /privacy lines that belong to the core rather than to a settings section.
const (
	privacyAttachments = "attachments"
	privacySchedules   = "schedules"
	privacyTasks       = "tasks"
	privacyVoice       = "voice"
)

// privacyOrder orders the lines /privacy adds below its opening paragraph. An entry is a core line
// above or the prefix of a section that adds lines of its own.
var privacyOrder = []string{
	modelSectionPrefix,
	privacyAttachments,
	nasSectionPrefix,
	notionSectionPrefix,
	googleSectionPrefix,
	privacySchedules,
	privacyTasks,
	webSectionPrefix,
	nsSectionPrefix,
	homeAssistantSectionPrefix,
	busSectionPrefix,
	locationSectionPrefix,
	privacyVoice,
	grafanaSectionPrefix,
	memorySectionPrefix,
	typeSafeSectionPrefix,
}

func (s *Service) privacyReply(ctx context.Context, principalID string) (string, error) {
	if s.conversation == nil {
		return PrivacyReply, nil
	}
	core := map[string]string{}
	if s.attachments != nil {
		core[privacyAttachments] = AttachmentPrivacyLine
	}
	if s.schedules != nil {
		core[privacySchedules] = SchedulePrivacyLine
	}
	if s.tasks != nil {
		core[privacyTasks] = TaskPrivacyLine
	}
	if s.transcriber != nil {
		core[privacyVoice] = VoicePrivacyLine
	}
	sections := sectionsFor(s.sections, principalID)
	reply := ConversationPrivacyReply
	for _, key := range privacyOrder {
		if line, found := core[key]; found {
			reply += privacyItem + line
			continue
		}
		for _, section := range sections {
			private, ok := section.(privacySection)
			if !ok || section.prefix() != key {
				continue
			}
			lines, err := private.privacy(ctx, principalID)
			if err != nil {
				return "", err
			}
			for _, line := range lines {
				reply += privacyItem + line
			}
		}
	}
	return reply, nil
}
