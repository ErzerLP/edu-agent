-- Deployment coverage is independent of learner activity and erasure.
-- Migration time alone does not prove full coverage; U-B startup sets it once.
ALTER TABLE model_usage_coverage ADD COLUMN full_started_at TIMESTAMPTZ;
-- Synchronous teaching/selector/probe calls have no durable mentor run lease.
-- Their configured HTTP deadline bounds recovery after a lost receipt/crash.
ALTER TABLE learning_model_calls ADD COLUMN lease_until TIMESTAMPTZ;
