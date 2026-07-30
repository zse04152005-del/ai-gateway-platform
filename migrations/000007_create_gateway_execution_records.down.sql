BEGIN;

DROP TRIGGER trg_route_attempts_status_event ON app.route_attempts;
DROP TRIGGER trg_gateway_requests_status_event ON app.gateway_requests;
DROP TRIGGER trg_route_attempts_transition ON app.route_attempts;
DROP TRIGGER trg_gateway_requests_transition ON app.gateway_requests;

DROP FUNCTION app.record_route_attempt_status_event();
DROP FUNCTION app.record_gateway_request_status_event();
DROP FUNCTION app.enforce_route_attempt_transition();
DROP FUNCTION app.enforce_gateway_request_transition();

DROP TABLE app.route_attempt_status_events;
DROP TABLE app.gateway_request_status_events;
DROP TABLE app.route_attempts;
DROP TABLE app.gateway_requests;

COMMIT;
