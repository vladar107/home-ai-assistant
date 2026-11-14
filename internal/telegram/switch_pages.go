package telegram

const (
	webSectionPrefix           = "web"
	nsSectionPrefix            = "ns"
	flightsSectionPrefix       = "flights"
	busSectionPrefix           = "bus"
	homeAssistantSectionPrefix = "ha"
	grafanaSectionPrefix       = "grafana"
	typeSafeSectionPrefix      = "typesafe"
	widgetSectionPrefix        = "widget"

	WebOnLine  = "Web access: on (the default). I can search the web through your own model subscription, read links, and check the weather."
	WebOffLine = "Web access: off. Turn it back on to let me search the web through your own model subscription, read links, and check the weather."

	NSOnLine  = "Trains (NS): on (the default). I can plan trips, list departures, and report disruptions from live NS data."
	NSOffLine = "Trains (NS): off. Turn it back on to let me plan trips, list departures, and report disruptions from live NS data."

	FlightsOnLine  = "Flight status: on (the default). I can look up a flight's times, delay, gate, and status by its number, and follow the flights of your tracked trips on their day of travel, with data from AeroDataBox (aerodatabox.com)."
	FlightsOffLine = "Flight status: off. Turn it back on to let me look up flights by their number and follow the flights of your tracked trips, with data from AeroDataBox (aerodatabox.com)."

	BusOnLine  = "Buses: on (the default). I can show live departures, direct buses, and disruption notices from OVapi."
	BusOffLine = "Buses: off. Turn it back on to let me show live departures, direct buses, and disruption notices from OVapi."
	// BusPlanningLine credits the planner, as Transitous's terms ask.
	BusPlanningLine = "Trips at a later time or with changes are planned by Transitous (transitous.org/sources), using open transit data and © OpenStreetMap contributors."

	HomeAssistantOnLine        = "Home (Home Assistant): on (the default). I can read and control the devices the household exposed to Assist, and propose confirmed actions for devices labelled for confirmation."
	HomeAssistantUncheckedLine = "Home actions are paused: I could not reach Home Assistant to check which devices are exposed."
	HomeAssistantConflictLine  = "Home actions are paused while these devices that need your confirmation are exposed to Assist; unexpose them in Home Assistant: "
	HomeAssistantPrivacyLine   = "Home: device names, areas, and states go to your model when you ask about the home."
	HomeAssistantOffLine       = "Home (Home Assistant): off. Turn it back on to let me read and control the devices the household exposed to Assist, and propose confirmed actions for devices labelled for confirmation."

	homeAssistantPausedAttention = "Home actions are paused; see Home & files."

	GrafanaOnLine      = "Monitoring (Grafana): on (the default). I can read the household's dashboards, query metrics, and search container logs."
	GrafanaOffLine     = "Monitoring (Grafana): off. Turn it back on to let me read the household's dashboards, query metrics, and search container logs."
	GrafanaPrivacyLine = "Monitoring: metrics and logs you ask about go to your model and can show other household members' activity."

	typeSafeDescription = "When on, a scrubbed copy of your recent messages, of the assistant's latest reply, of web pages and emails the assistant reads, and of proposed actions is sent to TypeSafe to warn you about actions you did not ask for and content that tries to instruct the assistant. File paths, links, email addresses, phone numbers, bank and card numbers, and keys are removed first; names and other free text may remain. Notion page contents and NAS file contents are never sent, and no reply is sent from a conversation that has read them, until /new; page titles, file names, and job names may be."

	TypeSafeOnLine  = "Safety checks (TypeSafe): on.\n" + typeSafeDescription
	TypeSafeOffLine = "Safety checks (TypeSafe): off.\n" + typeSafeDescription
	// TypeSafePrivacyLine is added to /privacy while the principal's safety checks are on.
	TypeSafePrivacyLine = "Safety checks: a scrubbed copy of messages, replies, outside content, and proposed actions goes to TypeSafe, never after Notion or NAS content."

	WidgetOnLine  = "Widgets: on. Proposals are shown as a widget, a small view Telegram draws in the chat, with the proposal, Confirm, Reject and Preview inside it and a one-line caption. A proposal of several changes is a list: tap a change to read it, untick the ones to leave out, and confirm the rest. A single change too long for a widget keeps its buttons."
	WidgetOffLine = "Widgets: off (the default). Proposals are shown with Confirm, Reject and Preview buttons. A widget is a small view Telegram draws in the chat; an app that cannot draw one shows only its caption."
	// WidgetPrivacyLine is added to /privacy while the principal has widgets on.
	WidgetPrivacyLine = "Widgets: what they show is drawn by a small program Telegram runs in the chat, which reaches nothing outside Telegram."
)

// The on/off pages of the integrations that run on an operator key or are opt-in.
var (
	WebPage = SwitchPage{
		Integration: "web", Prefix: webSectionPrefix, Name: "Web", OnLine: WebOnLine, OffLine: WebOffLine,
		OnButton: "Turn on web access", OffButton: "Turn off web access",
		Event: "web.access.changed", HelpTopic: "web", PrivacyLine: WebPrivacyLine,
	}
	NSPage = SwitchPage{
		Integration: "ns", Prefix: nsSectionPrefix, Name: "Trains", OnLine: NSOnLine, OffLine: NSOffLine,
		OnButton: "Turn on trains", OffButton: "Turn off trains",
		Event: "ns.trains.changed", HelpTopic: "trains", PrivacyLine: NSPrivacyLine,
	}
	FlightsPage = SwitchPage{
		Integration: "flights", Prefix: flightsSectionPrefix, Name: "Flights", OnLine: FlightsOnLine, OffLine: FlightsOffLine,
		OnButton: "Turn on flight status", OffButton: "Turn off flight status",
		Event: "flights.changed", HelpTopic: "flights", PrivacyLine: FlightsPrivacyLine,
	}
	BusPage = SwitchPage{
		Integration: "bus", Prefix: busSectionPrefix, Name: "Buses", OnLine: BusOnLine, ExtraOnLines: []string{BusPlanningLine}, OffLine: BusOffLine,
		OnButton: "Turn on buses", OffButton: "Turn off buses",
		Event: "bus.changed", HelpTopic: "buses", PrivacyLine: BusPrivacyLine + " " + BusPlanningPrivacyLine,
	}
	// HomePage takes HomeDetails for the exposure check.
	HomePage = SwitchPage{
		Integration: "homeassistant", Prefix: homeAssistantSectionPrefix, Name: "Home", OnLine: HomeAssistantOnLine, OffLine: HomeAssistantOffLine,
		OnButton: "Turn on home", OffButton: "Turn off home",
		Event: "homeassistant.home.changed", HelpTopic: "home_assistant", PrivacyLine: HomeAssistantPrivacyLine,
	}
	// GrafanaPage is shown only to the principals the operator allowed.
	GrafanaPage = SwitchPage{
		Integration: "grafana", Prefix: grafanaSectionPrefix, Name: "Grafana", OnLine: GrafanaOnLine, OffLine: GrafanaOffLine,
		OnButton: "Turn on monitoring", OffButton: "Turn off monitoring",
		Event: "grafana.changed", HelpTopic: "grafana", PrivacyLine: GrafanaPrivacyLine,
	}
	TypeSafePage = SwitchPage{
		Integration: "typesafe", Prefix: typeSafeSectionPrefix, Name: "Safety checks", OnLine: TypeSafeOnLine, OffLine: TypeSafeOffLine,
		OnButton: "Turn on safety checks", OffButton: "Turn off safety checks",
		HelpTopic: "safety_checks", PrivacyLine: TypeSafePrivacyLine,
		SummaryState: func(on bool) string { return "safety checks " + onOff(on) },
	}
	WidgetPage = SwitchPage{
		Integration: "widgets", Prefix: widgetSectionPrefix, Name: "Widgets", OnLine: WidgetOnLine, OffLine: WidgetOffLine,
		OnButton: "Turn on widgets", OffButton: "Turn off widgets",
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
