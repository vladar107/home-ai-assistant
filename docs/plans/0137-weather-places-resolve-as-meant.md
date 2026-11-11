# 0137 — Weather places resolve to the place that was meant

Status: `done`

## Context

A scheduled report drew its day card for a village on another continent while its text described
the right city. Three things combined:

- The geocoder is asked in English only and matches a name only in the language it is asked in.
  A city written in its own language (`Wien`, `Firenze`) returns only obscure namesakes under
  English and the right city under its own language; `Roma` returns a country first. `geocode`
  takes the first candidate.
- A qualifier that matches no candidate is ignored: `pickPlace` falls back to the first
  candidate, so `Wien, Austria` still lands on a namesake elsewhere.
- The place lookup exists three times and differs. `weather_forecast` and weather watches
  (`FindPlace`) try the user's saved places first; the forecast also accepts coordinates;
  `weather_card` only geocodes. The model saw the wrong place in the card's result, checked the
  forecast by coordinates instead, and sent the wrong picture with a disclaimer.

## Goal

A place name means the same place in every weather tool, a well-known city resolves whatever
language it is written in, and a qualifier is never silently ignored.

## Decisions

- One resolver serves `weather_forecast`, `weather_card`, and `FindPlace`: saved places first,
  then geocoding, and coordinates where the tool takes them. It returns the place and the name to
  show for it.
- Card legs gain optional `latitude` and `longitude`, exclusive with `place`. A leg with
  coordinates needs a `label`, which also heads the card when it is the first leg; a saved place
  shows under its saved name.
- A strong match is a populated place of at least 100,000 people that either carries the name
  asked for or is the first candidate of its answer; the geocoder puts a city first under its
  English name when it knows the name asked as another form of it. A country, a region, or a
  mountain is never strong. Names are equal without regard to case and surrounding space, after
  the suffix `baseName` drops; accents are not folded.
- A name is looked up again in other languages unless the English answer has a strong match. The
  retry asks a fixed list (`de`, `fr`, `es`, `it`, `pt`, `nl`, `pl`, `ru`) in parallel under its
  own five-second limit, and is skipped for a name that contains a digit, which is a street
  address.
- From the retry only a strong match that carries the name asked may replace the English
  answer, and the most populous one wins. Otherwise the English answer stands as it does
  today, with its first candidate, so a small town, a hamlet, or an island resolves as before.
  Two cases differ: when that first candidate is a country or a region the first populated place
  is taken, and when it is another feature that is no settlement, such as a mountain, a
  populated place of equal name is taken if there is one. A retry that fails or times out leaves the English answer,
  and the result then says the lookup was incomplete and which place was used. When English
  finds nothing, only a candidate of equal name from the retry is taken, the most populous one;
  otherwise the name was not found.
- A qualifier must match, by country code or region, not by how a language spells them. Each
  part is resolved to a country or a first-level region: from a few common short forms the
  geocoder does not know, from the candidates' own country and region names in every language
  asked, and otherwise by looking the last part up, in English and the retry's languages under
  one five-second limit, accepting only a country or a first-level region of that name. The parts then narrow the candidates
  of all answers in order, so a region and a country together pick the namesake in that region,
  and the choice above is made among what is left. With a qualifier more candidates are asked
  for, so a namesake in the named region is among them. A lookup that fails is reported as a
  failure, not as a place that does not exist, and so is a qualified name whose own lookup was
  incomplete. When a part does not resolve or matches nothing, the tool refuses, names the
  regions and countries it did find, and asks for the English name with its country or for
  coordinates. A weather watch is never created from an incomplete lookup. Later address parts are tried only
  when the first part finds nothing at all, as today; a qualifier is never taken for the place.
- The geocoder's answers are kept in memory for a day, by name and language, so the extra
  lookups are paid once per name; the qualifier is applied after the cache. The cache has an
  entry limit, since names come from the model, and never keeps the answers of a retry that
  failed or timed out. Saved places and coordinates do not pass through it.
- The geocoder stays Open-Meteo. Another geocoder was considered and left out: a larger change
  with its own failure modes.
- Redrawing a card within a turn was considered and left out. It changes the one-image rule that
  charts share and lets two parallel calls overwrite each other silently; with a strict lookup
  and coordinates the model has a way to get the place right the first time.
- The tool timeouts keep their values. They cover the usual case; a lookup that outlasts them is
  cut off by the tool's deadline and reported as the weather service not answering in time. The
  comments on the lookup limit and the timeouts are reworded to what now holds.

## Approach

1. Geocoding: parse population and feature fields, the language retry, the choice among answers,
   the strict qualifier with its refusal text, and the in-memory cache.
2. Shared resolution: one function for the three callers; the card's header, default label, and
   per-leg caches work from what it returns; the leg schema and tool guidance follow.
3. Tests that pin the old behaviour are changed on purpose, such as a made-up qualifier that
   used to succeed.
4. Documentation: the weather part of `README.md` and the in-bot help where they describe places.

This is one piece of work: the stages edit the same files.

## Files affected

- `internal/connectors/web/weather.go`, `precipitation.go`, `card.go`, `tools.go`, `service.go`,
  and their tests
- `internal/watch` tests where a watch is created on a place name
- `README.md`

## Validation

- Integration tests against a fake geocoder that answers by language:
  - a city in its own language resolves to the populous match;
  - a name whose English answer starts with a country resolves to the city;
  - a strong English match makes no further request;
  - a name the English answer lists first under its English name resolves to that city, and a
    mountain of equal name is not chosen over it;
  - a qualifier in English matches an answer found in another language, also when a look-alike
    in that country is in the English answer;
  - a region and a country together pick the namesake in that region;
  - an unmatched qualifier refuses and lists what was found;
  - a hamlet without population still resolves, and a small town keeps its English answer when
    a larger namesake below the bar exists in another language;
  - a qualifier that is itself a town's name is not taken for a country and refuses;
  - a street address makes no retry before its town is tried;
  - a retry that fails or times out keeps the English answer inside the tool's timeout;
  - a second lookup of the same name makes no request, also under a different qualifier, and a
    lookup after a failed retry asks again.
- A saved place name resolves to the same coordinates in the forecast, the card, and a weather
  watch; a leg with coordinates renders under its label; a leg with both a place and coordinates,
  or coordinates without a label, is refused.
- The configured format, module, test, vet, build, and plan checks.
- Before deployment, by hand against the real geocoder: the configured home place resolves to
  the same coordinates as before.
- Before landing, against the real geocoder with neutral names: cities in their own language,
  qualified names, and a made-up qualifier resolve as the tests expect.
- After deployment: a scheduled report for a city abroad, written in its own language.

## Risks and open questions

- A qualifier the geocoder does not know as a country or region (`Holland`) is now refused where
  it used to work by accident. The refusal lists the countries found so the model can retry.
- The language list is arbitrary. A name in another script or language still needs its English
  form or coordinates.
- A name below the strong-match bar costs eight more requests the first time it is asked.
- A town below the bar that is written in another language's form can lose to an English
  look-alike; its English name or a qualifier settles it.
- The first English candidate counts as strong when it is a large city, also when its name only
  resembles the one asked. That is what the lookup does today for every name.
- A small town still loses to a namesake city of 100,000 or more, as it mostly does today. A
  qualifier or a saved place settles it.
- Creating a weather watch on a place name can now be refused where it used to pick a namesake.
- A card whose place is still wrong cannot be redrawn in the same turn.
