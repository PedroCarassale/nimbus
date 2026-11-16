package concurrency

import "github.com/redis/go-redis/v9"

// admitScript is the atomic admission step.
//
// KEYS: slots, queue, global, exec hash, fifo sequence
// ARGV: executionId, functionId, maxConcurrency, queueMax, globalMax,
//
//	nowMs, leaseMs, queueLeaseMs, metaTTLMs, execKeyPrefix
//
// Reply: {status, position} where status is acquired, queued, rejected, or cancelled.
const admitLua = `
local slots, queue, global, execKey, seqKey = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5]
local executionId = ARGV[1]
local functionId = ARGV[2]
local maxConc = tonumber(ARGV[3])
local queueMax = tonumber(ARGV[4])
local globalMax = tonumber(ARGV[5])
local now = tonumber(ARGV[6])
local lease = tonumber(ARGV[7])
local queueLease = tonumber(ARGV[8])
local metaTTL = tonumber(ARGV[9])
local execPrefix = ARGV[10]
local expiry = now + lease

local function as_array(v)
  if type(v) == 'string' then
    return {v}
  end
  if type(v) ~= 'table' then
    return {}
  end
  return v
end

local function sweep_slots()
  redis.call('ZREMRANGEBYSCORE', slots, '-inf', tostring(now))
  redis.call('ZREMRANGEBYSCORE', global, '-inf', tostring(now))
end

local function sweep_queue()
  local members = as_array(redis.call('ZRANGE', queue, 0, -1))
  for _, member in ipairs(members) do
    if member ~= executionId then
      local hb = redis.call('HGET', execPrefix .. member, 'heartbeat')
      local hbN = tonumber(hb) or 0
      if now - hbN >= queueLease then
        redis.call('ZREM', queue, member)
        local st = redis.call('HGET', execPrefix .. member, 'status')
        if st == 'queued' or st == 'pending' or st == 'cancelled' or not st then
          redis.call('DEL', execPrefix .. member)
        end
      end
    end
  end
end

local function present(v)
  return v and v ~= false
end

sweep_slots()

if redis.call('EXISTS', execKey) == 0 then
  redis.call('HSET', execKey, 'functionId', functionId)
  redis.call('HSET', execKey, 'status', 'pending')
  redis.call('HSET', execKey, 'cancelled', 'false')
  redis.call('HSET', execKey, 'heartbeat', tostring(now))
  redis.call('PEXPIRE', execKey, metaTTL)
else
  redis.call('HSET', execKey, 'functionId', functionId)
  redis.call('HSET', execKey, 'heartbeat', tostring(now))
  redis.call('PEXPIRE', execKey, metaTTL)
end

sweep_queue()

if present(redis.call('ZSCORE', slots, executionId)) then
  redis.call('ZADD', slots, tostring(expiry), executionId)
  redis.call('ZADD', global, tostring(expiry), executionId)
  redis.call('ZREM', queue, executionId)
  redis.call('HSET', execKey, 'status', 'running')
  return {'acquired', '0'}
end

local cancelled = redis.call('HGET', execKey, 'cancelled')
if cancelled == '1' or cancelled == 'true' then
  redis.call('ZREM', queue, executionId)
  redis.call('DEL', execKey)
  return {'cancelled', '0'}
end

local active = tonumber(redis.call('ZCARD', slots)) or 0
local globalActive = tonumber(redis.call('ZCARD', global)) or 0
local inQueue = present(redis.call('ZSCORE', queue, executionId))

local head = as_array(redis.call('ZRANGE', queue, 0, 0))
local headId = head[1]
local room = active < maxConc and globalActive < globalMax
local fair = (not headId) or headId == executionId

if room and fair then
  redis.call('ZREM', queue, executionId)
  redis.call('ZADD', slots, tostring(expiry), executionId)
  redis.call('ZADD', global, tostring(expiry), executionId)
  redis.call('HSET', execKey, 'status', 'running')
  redis.call('HSET', execKey, 'startedAt', tostring(now))
  return {'acquired', '0'}
end

if not inQueue then
  local qlen = tonumber(redis.call('ZCARD', queue)) or 0
  if qlen >= queueMax then
    local st = redis.call('HGET', execKey, 'status')
    if st == 'pending' then
      redis.call('DEL', execKey)
    end
    return {'rejected', '0'}
  end
  local ticket = redis.call('INCR', seqKey)
  redis.call('ZADD', queue, tostring(ticket), executionId)
  redis.call('HSET', execKey, 'status', 'queued')
  redis.call('HSET', execKey, 'enqueuedAt', tostring(now))
end

local pos = redis.call('ZRANK', queue, executionId)
if not pos or pos == false then
  pos = 0
end
return {'queued', tostring(pos)}
`

const releaseLua = `
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('ZREM', KEYS[3], ARGV[1])
redis.call('DEL', KEYS[4])
return 1
`

// cancelLua flags the execution and, when it is only waiting, removes it from
// the queue immediately so the next waiter can take the slot.
//
// KEYS[1] exec hash
// ARGV: slotsPrefix, queuePrefix, globalKey, executionId
const cancelLua = `
local execKey = KEYS[1]
local slotsPrefix, queuePrefix, globalKey, executionId = ARGV[1], ARGV[2], ARGV[3], ARGV[4]
local functionId = redis.call('HGET', execKey, 'functionId')
if not functionId or functionId == false then
  return {'missing'}
end
local queue = queuePrefix .. functionId
redis.call('HSET', execKey, 'cancelled', 'true')
local removed = tonumber(redis.call('ZREM', queue, executionId)) or 0
local status = redis.call('HGET', execKey, 'status')
if removed > 0 or status == 'queued' or status == 'pending' then
  redis.call('HSET', execKey, 'status', 'cancelled')
  return {'queued'}
end
return {'running'}
`

// leaveLua drops a waiter that gave up (timeout or caller cancel) without
// touching a slot that was already granted.
const leaveLua = `
local execKey, queue = KEYS[1], KEYS[2]
local executionId = ARGV[1]
local status = redis.call('HGET', execKey, 'status')
if status == 'queued' or status == 'pending' or status == 'cancelled' then
  redis.call('ZREM', queue, executionId)
  redis.call('DEL', execKey)
  return 1
end
return 0
`

// touchLua extends the lease of a live holder and drops expired peers.
const touchLua = `
local slots, queue, global, execKey = KEYS[1], KEYS[2], KEYS[3], KEYS[4]
local executionId = ARGV[1]
local now = tonumber(ARGV[2])
local lease = tonumber(ARGV[3])
local queueLease = tonumber(ARGV[4])
local metaTTL = tonumber(ARGV[5])
local execPrefix = ARGV[6]
local expiry = now + lease

local function as_array(v)
  if type(v) == 'string' then
    return {v}
  end
  if type(v) ~= 'table' then
    return {}
  end
  return v
end

redis.call('ZREMRANGEBYSCORE', slots, '-inf', tostring(now))
redis.call('ZREMRANGEBYSCORE', global, '-inf', tostring(now))

local members = as_array(redis.call('ZRANGE', queue, 0, -1))
for _, member in ipairs(members) do
  local hb = redis.call('HGET', execPrefix .. member, 'heartbeat')
  local hbN = tonumber(hb) or 0
  if now - hbN >= queueLease then
    redis.call('ZREM', queue, member)
    local st = redis.call('HGET', execPrefix .. member, 'status')
    if st == 'queued' or st == 'pending' or not st then
      redis.call('DEL', execPrefix .. member)
    end
  end
end

local held = redis.call('ZSCORE', slots, executionId)
if not held or held == false then
  return 0
end
redis.call('ZADD', slots, tostring(expiry), executionId)
redis.call('ZADD', global, tostring(expiry), executionId)
redis.call('HSET', execKey, 'heartbeat', tostring(now))
redis.call('HSET', execKey, 'status', 'running')
redis.call('PEXPIRE', execKey, metaTTL)
return 1
`

var (
	admitScript   = redis.NewScript(admitLua)
	releaseScript = redis.NewScript(releaseLua)
	cancelScript  = redis.NewScript(cancelLua)
	leaveScript   = redis.NewScript(leaveLua)
	touchScript   = redis.NewScript(touchLua)
)
