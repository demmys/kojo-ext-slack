// Package slackbot provides Slack Socket Mode integration for Kojo agents.
//
// It is the bot that used to be built into kojo, moved into this extension.
// The bot reaches kojo only through ChatManager and a handful of optional
// capability interfaces; package kojoclient implements them over kojo's
// ext-chat HTTP API.
package slackbot
