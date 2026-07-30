BEGIN;

-- A GatewayRequest records the model name supplied by an authenticated client.
-- Selection, rather than persistence, owns the decision whether that name has
-- an authorized and healthy deployment. Keeping this fact independent from the
-- mutable catalog also lets MODEL_UNAVAILABLE requests remain auditable.
ALTER TABLE app.gateway_requests
    DROP CONSTRAINT gateway_requests_logical_model_fk;

COMMENT ON COLUMN app.gateway_requests.logical_model IS
    'Validated client-supplied logical model fact; it may be absent from the current catalog and is resolved by routing';

COMMIT;
