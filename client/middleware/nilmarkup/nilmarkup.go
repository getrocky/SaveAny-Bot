// Package nilmarkup drops typed-nil reply markups from outgoing requests.
//
// A nil *tg.ReplyInlineMarkup stored in a tg.ReplyMarkupClass field is not an
// untyped nil interface, so gotd sets the reply_markup flag and then fails to
// encode the request with "can't encode replyInlineMarkup#b2b15770 as nil".
// That error is not a Telegram error, so the recovery middleware used to retry
// it with backoff for up to five minutes, blocking the handler that sent it.
package nilmarkup

import (
	"context"
	"reflect"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// IsNil reports whether m is nil or an interface holding a nil pointer.
func IsNil(m tg.ReplyMarkupClass) bool {
	if m == nil {
		return true
	}
	v := reflect.ValueOf(m)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// Sanitize clears typed-nil reply markups on requests that carry one.
func Sanitize(input bin.Encoder) {
	switch req := input.(type) {
	case *tg.MessagesSendMessageRequest:
		if req.ReplyMarkup != nil && IsNil(req.ReplyMarkup) {
			req.ReplyMarkup = nil
		}
	case *tg.MessagesEditMessageRequest:
		if req.ReplyMarkup != nil && IsNil(req.ReplyMarkup) {
			req.ReplyMarkup = nil
		}
	case *tg.MessagesSendMediaRequest:
		if req.ReplyMarkup != nil && IsNil(req.ReplyMarkup) {
			req.ReplyMarkup = nil
		}
	case *tg.MessagesEditInlineBotMessageRequest:
		if req.ReplyMarkup != nil && IsNil(req.ReplyMarkup) {
			req.ReplyMarkup = nil
		}
	}
}

type middleware struct{}

// New returns a middleware that removes typed-nil reply markups before sending.
func New() telegram.Middleware { return middleware{} }

func (middleware) Handle(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		Sanitize(input)
		return next.Invoke(ctx, input, output)
	}
}
