package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// The script increments and (re)arms the expiry atomically so a crash
// between INCR and PEXPIRE can never leave a counter that never resets.
var incrScript = redis.NewScript(`
local c = redis.call('INCR', KEYS[1])
if c == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {c, ttl}
`)

// Redis limits through a shared Redis; exact across replicas.
type Redis struct {
	client *redis.Client
}

// NewRedis connects using a redis:// or rediss:// URL.
func NewRedis(url string) (*Redis, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 500 * time.Millisecond
	opts.WriteTimeout = 500 * time.Millisecond
	return &Redis{client: redis.NewClient(opts)}, nil
}

func (r *Redis) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }
func (r *Redis) Close() error                   { return r.client.Close() }

func (r *Redis) Allow(ctx context.Context, key string, rule Rule) (Decision, error) {
	res, err := incrScript.Run(ctx, r.client, []string{"smtph:rl:" + key + "|" + rule.Window.String()},
		rule.Window.Milliseconds()).Slice()
	if err != nil {
		return Decision{}, err
	}
	if len(res) != 2 {
		return Decision{}, fmt.Errorf("unexpected redis script result %v", res)
	}
	count, _ := res[0].(int64)
	ttl, _ := res[1].(int64)
	return Decision{Allowed: count <= int64(rule.Limit), RetryAfter: time.Duration(ttl) * time.Millisecond}, nil
}
