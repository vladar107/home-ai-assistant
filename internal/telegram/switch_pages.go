package telegram

import (
	"github.com/vladar107/home-ai-assistant/internal/locale"
)

const (
	webSectionPrefix           = "web"
	nsSectionPrefix            = "ns"
	flightsSectionPrefix       = "flights"
	busSectionPrefix           = "bus"
	homeAssistantSectionPrefix = "ha"
	grafanaSectionPrefix       = "grafana"
	typeSafeSectionPrefix      = "typesafe"
	widgetSectionPrefix        = "widget"
)

var (
	homeAssistantPausedAttention = locale.T("Home actions are paused; see Home & files.", "Действия с домом приостановлены; см. «Дом и файлы».")
	HomeAssistantUncheckedLine   = locale.T("Home actions are paused: I could not reach Home Assistant to check which devices are exposed.", "Действия с домом приостановлены: не удалось связаться с Home Assistant, чтобы проверить, какие устройства открыты для Assist.")
	HomeAssistantPrivacyLine     = locale.T("Home: device names, areas, and states go to your model when you ask about the home.", "Дом: названия устройств, помещения и состояния уходят твоей модели, когда ты спрашиваешь о доме.")
	TypeSafeOnLine               = locale.T("Safety checks (TypeSafe): on.\nWhen on, a scrubbed copy of your recent messages, of the assistant's latest reply, of web pages and emails the assistant reads, and of proposed actions is sent to TypeSafe to warn you about actions you did not ask for and content that tries to instruct the assistant. File paths, links, email addresses, phone numbers, bank and card numbers, and keys are removed first; names and other free text may remain. Notion page contents and NAS file contents are never sent, and no reply is sent from a conversation that has read them, until /new; page titles, file names, and job names may be.", "Проверки безопасности (TypeSafe): включены.\nКогда включено, обезличенная копия твоих последних сообщений, последнего ответа ассистента, веб-страниц и писем, которые читает ассистент, и предлагаемых действий отправляется в TypeSafe, чтобы предупредить тебя о действиях, не связанных с твоей просьбой, и о содержимом, которое пытается давать указания ассистенту. Сначала удаляются пути к файлам, ссылки, адреса электронной почты, номера телефонов, номера банковских карт и счетов, а также ключи; имена и другой произвольный текст могут остаться. Содержимое страниц Notion и файлов NAS не отправляется никогда, и ни один ответ не отправляется из разговора, который их читал, до /new; названия страниц, имена файлов и названия заданий могут отправляться.")
	TypeSafeOffLine              = locale.T("Safety checks (TypeSafe): off.\nWhen on, a scrubbed copy of your recent messages, of the assistant's latest reply, of web pages and emails the assistant reads, and of proposed actions is sent to TypeSafe to warn you about actions you did not ask for and content that tries to instruct the assistant. File paths, links, email addresses, phone numbers, bank and card numbers, and keys are removed first; names and other free text may remain. Notion page contents and NAS file contents are never sent, and no reply is sent from a conversation that has read them, until /new; page titles, file names, and job names may be.", "Проверки безопасности (TypeSafe): выключены.\nКогда включено, обезличенная копия твоих последних сообщений, последнего ответа ассистента, веб-страниц и писем, которые читает ассистент, и предлагаемых действий отправляется в TypeSafe, чтобы предупредить тебя о действиях, не связанных с твоей просьбой, и о содержимом, которое пытается давать указания ассистенту. Сначала удаляются пути к файлам, ссылки, адреса электронной почты, номера телефонов, номера банковских карт и счетов, а также ключи; имена и другой произвольный текст могут остаться. Содержимое страниц Notion и файлов NAS не отправляется никогда, и ни один ответ не отправляется из разговора, который их читал, до /new; названия страниц, имена файлов и названия заданий могут отправляться.")
	WebOnLine                    = locale.T("Web access: on (the default). I can search the web through your own model subscription, read links, and check the weather.", "Доступ в интернет: включён (по умолчанию). Я могу искать в интернете через твою собственную подписку на модель, читать ссылки и проверять погоду.")
	WebOffLine                   = locale.T("Web access: off. Turn it back on to let me search the web through your own model subscription, read links, and check the weather.", "Доступ в интернет: выключен. Включи его снова, и я смогу искать в интернете через твою собственную подписку на модель, читать ссылки и проверять погоду.")
	NSOnLine                     = locale.T("Trains (NS): on (the default). I can plan trips, list departures, and report disruptions from live NS data.", "Поезда (NS): включены (по умолчанию). Я могу планировать поездки, показывать отправления и сообщать о сбоях по актуальным данным NS.")
	NSOffLine                    = locale.T("Trains (NS): off. Turn it back on to let me plan trips, list departures, and report disruptions from live NS data.", "Поезда (NS): выключены. Включи их снова, и я смогу планировать поездки, показывать отправления и сообщать о сбоях по актуальным данным NS.")
	FlightsOnLine                = locale.T("Flight status: on (the default). I can look up a flight's times, delay, gate, and status by its number, and follow the flights of your tracked trips on their day of travel, with data from AeroDataBox (aerodatabox.com).", "Статус рейсов: включён (по умолчанию). Я могу узнать по номеру рейса время, задержку, выход и статус, а в день поездки следить за рейсами твоих отслеживаемых поездок; данные AeroDataBox (aerodatabox.com).")
	FlightsOffLine               = locale.T("Flight status: off. Turn it back on to let me look up flights by their number and follow the flights of your tracked trips, with data from AeroDataBox (aerodatabox.com).", "Статус рейсов: выключен. Включи его снова, и я смогу искать рейсы по номеру и следить за рейсами твоих отслеживаемых поездок; данные AeroDataBox (aerodatabox.com).")
	BusOnLine                    = locale.T("Buses: on (the default). I can show live departures, direct buses, and disruption notices from OVapi.", "Автобусы: включены (по умолчанию). Я могу показывать актуальные отправления, прямые автобусы и уведомления о сбоях из OVapi.")
	BusOffLine                   = locale.T("Buses: off. Turn it back on to let me show live departures, direct buses, and disruption notices from OVapi.", "Автобусы: выключены. Включи их снова, и я смогу показывать актуальные отправления, прямые автобусы и уведомления о сбоях из OVapi.")
	// BusPlanningLine credits the planner, as Transitous's terms ask.
	BusPlanningLine      = locale.T("Trips at a later time or with changes are planned by Transitous (transitous.org/sources), using open transit data and © OpenStreetMap contributors.", "Поездки на более позднее время или с пересадками планирует Transitous (transitous.org/sources) по открытым данным о транспорте и © участники OpenStreetMap.")
	HomeAssistantOnLine  = locale.T("Home (Home Assistant): on (the default). I can read and control the devices the household exposed to Assist, and propose confirmed actions for devices labelled for confirmation.", "Дом (Home Assistant): включён (по умолчанию). Я могу видеть и управлять устройствами, открытыми для Assist, а для устройств с пометкой «требует подтверждения» предлагать действия с подтверждением.")
	HomeAssistantOffLine = locale.T("Home (Home Assistant): off. Turn it back on to let me read and control the devices the household exposed to Assist, and propose confirmed actions for devices labelled for confirmation.", "Дом (Home Assistant): выключен. Включи его снова, и я смогу видеть и управлять устройствами, открытыми для Assist, а для устройств с пометкой «требует подтверждения» предлагать действия с подтверждением.")
	GrafanaOnLine        = locale.T("Monitoring (Grafana): on (the default). I can read the household's dashboards, query metrics, and search container logs.", "Мониторинг (Grafana): включён (по умолчанию). Я могу читать дашборды твоего дома, запрашивать метрики и искать по логам контейнеров.")
	GrafanaOffLine       = locale.T("Monitoring (Grafana): off. Turn it back on to let me read the household's dashboards, query metrics, and search container logs.", "Мониторинг (Grafana): выключен. Включи его снова, и я смогу читать дашборды твоего дома, запрашивать метрики и искать по логам контейнеров.")
	GrafanaPrivacyLine   = locale.T("Monitoring: metrics and logs you ask about go to your model and can show other household members' activity.", "Мониторинг: метрики и логи, о которых ты спрашиваешь, уходят твоей модели и могут показывать активность других членов семьи.")
	// TypeSafePrivacyLine is added to /privacy while the principal's safety checks are on.
	TypeSafePrivacyLine = locale.T("Safety checks: a scrubbed copy of messages, replies, outside content, and proposed actions goes to TypeSafe, never after Notion or NAS content.", "Проверки безопасности: очищенная копия сообщений, ответов, внешнего содержимого и предлагаемых действий уходит в TypeSafe, и никогда — после чтения содержимого Notion или NAS.")
	WidgetOnLine        = locale.T("Widgets: on. Proposals are shown as a widget, a small view Telegram draws in the chat, with the proposal, Confirm, Reject and Preview inside it and a one-line caption. A proposal of several changes is a list: tap a change to read it, untick the ones to leave out, and confirm the rest. A single change too long for a widget keeps its buttons.", "Виджеты: включены. Предложения показываются как виджет, небольшое окно, которое Telegram рисует в чате: с самим предложением, кнопками «Подтвердить», «Отклонить» и «Предпросмотр» внутри и однострочной подписью. Предложение из нескольких изменений показывается списком: коснись изменения, чтобы прочитать его, сними отметки с тех, которые нужно пропустить, и подтверди остальные. Слишком длинное для виджета одиночное изменение сохраняет свои кнопки.")
	WidgetOffLine       = locale.T("Widgets: off (the default). Proposals are shown with Confirm, Reject and Preview buttons. A widget is a small view Telegram draws in the chat; an app that cannot draw one shows only its caption.", "Виджеты: выключены (по умолчанию). Предложения показываются с кнопками «Подтвердить», «Отклонить» и «Предпросмотр». Виджет — это небольшое окно, которое Telegram рисует в чате; приложение, которое не может его нарисовать, показывает только подпись.")
	// WidgetPrivacyLine is added to /privacy while the principal has widgets on.
	WidgetPrivacyLine = locale.T("Widgets: what they show is drawn by a small program Telegram runs in the chat, which reaches nothing outside Telegram.", "Виджеты: то, что они показывают, рисует небольшая программа, которую Telegram запускает в чате и которая не имеет доступа ни к чему вне Telegram.")
)

// The on/off pages of the integrations that run on an operator key or are opt-in.
var (
	WebPage = SwitchPage{
		Integration: "web", Prefix: webSectionPrefix, Name: locale.T("Web", "Интернет"), OnLine: WebOnLine, OffLine: WebOffLine,
		OnButton: locale.T("Turn on web access", "Включить доступ в интернет"), OffButton: locale.T("Turn off web access", "Выключить доступ в интернет"),
		Event: "web.access.changed", HelpTopic: "web", PrivacyLine: WebPrivacyLine,
	}
	NSPage = SwitchPage{
		Integration: "ns", Prefix: nsSectionPrefix, Name: locale.T("Trains", "Поезда"), OnLine: NSOnLine, OffLine: NSOffLine,
		OnButton: locale.T("Turn on trains", "Включить поезда"), OffButton: locale.T("Turn off trains", "Выключить поезда"),
		Event: "ns.trains.changed", HelpTopic: "trains", PrivacyLine: NSPrivacyLine,
	}
	FlightsPage = SwitchPage{
		Integration: "flights", Prefix: flightsSectionPrefix, Name: locale.T("Flights", "Рейсы"), OnLine: FlightsOnLine, OffLine: FlightsOffLine,
		OnButton: locale.T("Turn on flight status", "Включить статус рейсов"), OffButton: locale.T("Turn off flight status", "Выключить статус рейсов"),
		Event: "flights.changed", HelpTopic: "flights", PrivacyLine: FlightsPrivacyLine,
	}
	BusPage = SwitchPage{
		Integration: "bus", Prefix: busSectionPrefix, Name: locale.T("Buses", "Автобусы"), OnLine: BusOnLine, ExtraOnLines: []locale.Text{BusPlanningLine}, OffLine: BusOffLine,
		OnButton: locale.T("Turn on buses", "Включить автобусы"), OffButton: locale.T("Turn off buses", "Выключить автобусы"),
		Event: "bus.changed", HelpTopic: "buses", PrivacyLine: BusPrivacyLine, PrivacyMore: []locale.Text{BusPlanningPrivacyLine},
	}
	// HomePage takes HomeDetails for the exposure check.
	HomePage = SwitchPage{
		Integration: "homeassistant", Prefix: homeAssistantSectionPrefix, Name: locale.T("Home", "Дом"), OnLine: HomeAssistantOnLine, OffLine: HomeAssistantOffLine,
		OnButton: locale.T("Turn on home", "Включить дом"), OffButton: locale.T("Turn off home", "Выключить дом"),
		Event: "homeassistant.home.changed", HelpTopic: "home_assistant", PrivacyLine: HomeAssistantPrivacyLine,
	}
	// GrafanaPage is shown only to the principals the operator allowed.
	GrafanaPage = SwitchPage{
		Integration: "grafana", Prefix: grafanaSectionPrefix, Name: locale.T("Grafana", "Grafana"), OnLine: GrafanaOnLine, OffLine: GrafanaOffLine,
		OnButton: locale.T("Turn on monitoring", "Включить мониторинг"), OffButton: locale.T("Turn off monitoring", "Выключить мониторинг"),
		Event: "grafana.changed", HelpTopic: "grafana", PrivacyLine: GrafanaPrivacyLine,
	}
	TypeSafePage = SwitchPage{
		Integration: "typesafe", Prefix: typeSafeSectionPrefix, Name: locale.T("Safety checks", "Проверки безопасности"), OnLine: TypeSafeOnLine, OffLine: TypeSafeOffLine,
		OnButton: locale.T("Turn on safety checks", "Включить проверки безопасности"), OffButton: locale.T("Turn off safety checks", "Выключить проверки безопасности"),
		HelpTopic: "safety_checks", PrivacyLine: TypeSafePrivacyLine,
		SummaryOn: locale.T("safety checks on", "проверки безопасности включены"), SummaryOff: locale.T("safety checks off", "проверки безопасности выключены"),
	}
	WidgetPage = SwitchPage{
		Integration: "widgets", Prefix: widgetSectionPrefix, Name: locale.T("Widgets", "Виджеты"), OnLine: WidgetOnLine, OffLine: WidgetOffLine,
		OnButton: locale.T("Turn on widgets", "Включить виджеты"), OffButton: locale.T("Turn off widgets", "Выключить виджеты"),
		Event: "widgets.changed", HelpTopic: "widgets", PrivacyLine: WidgetPrivacyLine,
	}
)

// SwitchPageFor is the on/off page of the named integration.
func SwitchPageFor(integration string) (SwitchPage, bool) {
	for _, page := range []SwitchPage{WebPage, NSPage, FlightsPage, BusPage, HomePage, GrafanaPage, TypeSafePage, WidgetPage} {
		if page.Integration == integration {
			return page, true
		}
	}
	return SwitchPage{}, false
}
