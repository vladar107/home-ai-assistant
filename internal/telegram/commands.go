package telegram

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/connection"
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	// privacyItem starts each line /privacy adds below its opening paragraph.
	privacyItem = "\n• "
)

var (
	// Asking for a file uploads the whole file to Telegram, which reading one as text does not.
	SentFilePrivacyLine = locale.T("A NAS file reaches Telegram only when you ask for it.", "Файл с NAS попадает в Telegram, только когда ты его запрашиваешь.")

	StatusReply             = locale.T("Home Agent is online. Telegram access is active. This command does not use a model.", "Home Agent на связи. Доступ к Telegram активен. Эта команда не использует модель.")
	ConversationStatusReply = locale.T("Home Agent is online. Telegram access is active. This command does not use a model. Natural-language conversation is enabled.", "Home Agent на связи. Доступ к Telegram активен. Эта команда не использует модель. Можно писать обычными сообщениями.")
	PrivacyReply            = locale.T("Privacy mode is strict-local. Local file and photo content stays on the home system unless you explicitly approve a scoped disclosure. This command does not use a model.", "Режим приватности: строго локальный. Содержимое локальных файлов и фото остаётся в домашней системе, пока ты явно не одобришь ограниченную передачу. Эта команда не использует модель.")
	NothingToCancelReply    = locale.T("There is no response to cancel.", "Нет ответа, который можно остановить.")

	UnsupportedAudioReply      = locale.T("I cannot listen to voice messages or audio yet. Send text, a photo, or a file and I will work with that.", "Голосовые сообщения и аудио я пока слушать не умею. Отправь текст, фото или файл, и я с этим поработаю.")
	UnsupportedStickerReply    = locale.T("I cannot read stickers. Send text, a photo, or a file and I will work with that.", "Стикеры я прочитать не могу. Отправь текст, фото или файл, и я с этим поработаю.")
	UnsupportedAttachmentReply = locale.T("I cannot accept photos or files in this conversation.", "В этом разговоре я не могу принимать фото и файлы.")
	AttachmentUnavailableText  = locale.T("I could not download that from Telegram. Please send it again.", "Не удалось скачать это из Telegram. Отправь ещё раз.")
	ConversationFullReply      = locale.T("This conversation is already holding as many files as I can keep. Use /new to start a fresh one.", "В этом разговоре уже столько файлов, сколько я могу хранить. Начни новый разговор командой /new.")

	ConversationPrivacyReply = locale.T("Your messages go to your own model subscription. Conversation history stays on the home system, readable only by your assistant, and is deleted 90 days after the conversation ends; /new starts fresh. Integrations can be turned off in /settings.", "Твои сообщения уходят в твою собственную подписку на модель. История разговоров хранится в домашней системе, доступна только твоему ассистенту и удаляется через 90 дней после окончания разговора; /new начинает разговор заново. Интеграции можно отключить в /settings.")
	CancelledReply           = locale.T("Stopped the current response.", "Текущий ответ остановлен.")
	NewSessionReply          = locale.T("Started a fresh conversation.", "Начат новый разговор.")
	NoSessionReply           = locale.T("There is no conversation to reset yet.", "Пока нет разговора, который можно сбросить.")
	HelpHeading              = locale.T("Available commands:", "Доступные команды:")
	HelpConversationLine     = locale.T("You can otherwise talk to me normally, and reply to one of my earlier answers to continue that conversation.", "В остальном можно просто разговаривать со мной, а чтобы продолжить прежний разговор, ответь на одно из моих прошлых сообщений.")
	InvalidMessageReply      = locale.T("I could not take that message. A message can hold at most 4096 characters; split a longer one and send the parts.", "Не удалось принять это сообщение. В сообщении может быть не больше 4096 символов; раздели длинное на части и отправь их.")
	AttachmentPrivacyLine    = locale.T("Photos and files you send are deleted, from the history too, when their conversation ends: on /new, on a switch to another, or when you close its tab.", "Фото и файлы, которые ты отправляешь, удаляются, в том числе из истории, когда заканчивается их разговор: по /new, при переходе к другому разговору или когда ты закрываешь его вкладку.")
	// Only the search stays with the provider the conversation already uses, so each other place a
	// web request goes is named.
	WebPrivacyLine         = locale.T("Web: searches go to your model provider, pages are fetched from home, and places go to Open-Meteo for weather.", "Интернет: поисковые запросы уходят твоему поставщику модели, страницы загружаются из дома, а места отправляются в Open-Meteo для прогноза погоды.")
	NSPrivacyLine          = locale.T("Trains: stations and times go to NS.", "Поезда: станции и время уходят в NS.")
	FlightsPrivacyLine     = locale.T("Flights: the flight numbers and dates you ask about, and those of your tracked trips' flights, go to AeroDataBox through RapidAPI; booking references, names, and seats never do.", "Рейсы: номера и даты рейсов, о которых ты спрашиваешь, и рейсов твоих отслеживаемых поездок уходят в AeroDataBox через RapidAPI; коды бронирования, имена и места не отправляются никогда.")
	BusPrivacyLine         = locale.T("Buses: stops go to OVapi over plain HTTP.", "Автобусы: остановки отправляются в OVapi по обычному HTTP.")
	BusPlanningPrivacyLine = locale.T("Planned trips go to Transitous.", "Планируемые поездки уходят в Transitous.")
	UnsupportedVideoReply  = locale.T("I cannot watch video yet. Send text, a photo, or a file and I will work with that.", "Видео я пока смотреть не умею. Отправь текст, фото или файл, и я с этим поработаю.")
)

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// menuCommand is one entry of the slash menu and whether the deployment offers it.
type menuCommand struct {
	command     string
	description locale.Text
	offered     bool
}

// typedCommands answer when typed but are left out of the slash menu; the help reply and /settings
// name them.
var typedCommands = []menuCommand{
	{"status", locale.T("Show whether Home Agent is online, your model, and your subscription usage", "Показать, на связи ли Home Agent, твою модель и использование подписки"), true},
	{"privacy", locale.T("Show the current privacy mode", "Показать текущий режим приватности"), true},
}

// menuCommands is the slash menu in a language: the offered entries, which callers list
// alphabetically so a command is found where the user looks for it.
func menuCommands(language locale.Language, entries []menuCommand) []BotCommand {
	var commands []BotCommand
	for _, entry := range entries {
		if entry.offered {
			commands = append(commands, BotCommand{Command: entry.command, Description: entry.description.In(language)})
		}
	}
	return commands
}

var (
	connectInstructions = locale.N(
		"Open %[2]s and enter the code %[3]s. The code expires in %[1]d minute. I will confirm here once your subscription is connected.",
		"Open %[2]s and enter the code %[3]s. The code expires in %[1]d minutes. I will confirm here once your subscription is connected.",
		"Открой %[2]s и введи код %[3]s. Код действует %[1]d минуту. Я сообщу здесь, когда твоя подписка будет подключена.",
		"Открой %[2]s и введи код %[3]s. Код действует %[1]d минуты. Я сообщу здесь, когда твоя подписка будет подключена.",
		"Открой %[2]s и введи код %[3]s. Код действует %[1]d минут. Я сообщу здесь, когда твоя подписка будет подключена.",
	)
	modelConnectedLine = locale.T("Model subscription: ChatGPT account ending in %s, access token expires %s.", "Подписка на модель: аккаунт ChatGPT, оканчивается на %s, токен доступа действует до %s.")
)

func ConnectInstructions(ctx context.Context, ceremony connection.Ceremony) string {
	minutes := int(math.Ceil(time.Until(ceremony.ExpiresAt).Minutes()))
	if minutes < 1 {
		minutes = 1
	}
	return connectInstructions.Format(ctx, minutes, ceremony.VerificationURL, ceremony.UserCode)
}

func ModelConnectedLine(ctx context.Context, status connection.Status) string {
	suffix := status.AccountID
	if len(suffix) > 4 {
		suffix = suffix[len(suffix)-4:]
	}
	return modelConnectedLine.Format(ctx, suffix, status.ExpiresAt.UTC().Format(time.RFC3339))
}

func ParseCommand(text, botUsername string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", false
	}

	command, target, targeted := strings.Cut(fields[0], "@")
	if targeted {
		if target == "" || botUsername == "" || !strings.EqualFold(target, strings.TrimPrefix(botUsername, "@")) {
			return "", false
		}
	}
	return strings.TrimPrefix(strings.ToLower(command), "/"), true
}
