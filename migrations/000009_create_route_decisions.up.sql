BEGIN;

CREATE TABLE app.route_decisions (
    request_id text NOT NULL,
    decision_no integer NOT NULL,
    next_attempt_no integer NOT NULL,
    outcome text NOT NULL,
    filter_policy_version text NOT NULL,
    candidate_decisions jsonb NOT NULL,
    route_policy_version text,
    policy_decision jsonb,
    retry_policy_version text,
    retry_decision jsonb,
    selected_deployment_id uuid,
    decided_at timestamptz NOT NULL,
    PRIMARY KEY (request_id, decision_no),
    CONSTRAINT route_decisions_request_fk FOREIGN KEY (request_id)
        REFERENCES app.gateway_requests (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT route_decisions_selected_deployment_fk FOREIGN KEY (selected_deployment_id)
        REFERENCES app.deployments (id)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT route_decisions_number_positive CHECK (decision_no > 0),
    CONSTRAINT route_decisions_next_attempt_positive CHECK (next_attempt_no > 0),
    CONSTRAINT route_decisions_outcome_valid CHECK (
        outcome IN ('selected', 'no_candidate', 'selection_failed')
    ),
    CONSTRAINT route_decisions_filter_policy_version_format CHECK (
        char_length(filter_policy_version) BETWEEN 1 AND 128
        AND filter_policy_version ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
    ),
    CONSTRAINT route_decisions_candidate_decisions_valid CHECK (
        jsonb_typeof(candidate_decisions) = 'array'
        AND jsonb_array_length(candidate_decisions) <= 256
        AND octet_length(candidate_decisions::text) <= 65536
    ),
    CONSTRAINT route_decisions_route_policy_version_format CHECK (
        route_policy_version IS NULL
        OR (
            char_length(route_policy_version) BETWEEN 1 AND 128
            AND route_policy_version ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
        )
    ),
    CONSTRAINT route_decisions_policy_decision_valid CHECK (
        policy_decision IS NULL
        OR (
            jsonb_typeof(policy_decision) = 'object'
            AND octet_length(policy_decision::text) <= 16384
        )
    ),
    CONSTRAINT route_decisions_retry_policy_version_format CHECK (
        retry_policy_version IS NULL
        OR (
            char_length(retry_policy_version) BETWEEN 1 AND 128
            AND retry_policy_version ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
        )
    ),
    CONSTRAINT route_decisions_retry_decision_valid CHECK (
        retry_decision IS NULL
        OR (
            jsonb_typeof(retry_decision) = 'object'
            AND octet_length(retry_decision::text) <= 16384
        )
    ),
    CONSTRAINT route_decisions_policy_pair_valid CHECK (
        (route_policy_version IS NULL) = (policy_decision IS NULL)
    ),
    CONSTRAINT route_decisions_retry_pair_valid CHECK (
        (retry_policy_version IS NULL) = (retry_decision IS NULL)
    ),
    CONSTRAINT route_decisions_selection_valid CHECK (
        (
            outcome = 'selected'
            AND selected_deployment_id IS NOT NULL
            AND policy_decision IS NOT NULL
        )
        OR (
            outcome IN ('no_candidate', 'selection_failed')
            AND selected_deployment_id IS NULL
            AND policy_decision IS NULL
        )
    )
);

CREATE TABLE app.route_retry_decisions (
    request_id text NOT NULL,
    attempt_no integer NOT NULL,
    retry_policy_version text NOT NULL,
    retry_decision jsonb NOT NULL,
    decided_at timestamptz NOT NULL,
    PRIMARY KEY (request_id, attempt_no),
    CONSTRAINT route_retry_decisions_attempt_fk FOREIGN KEY (request_id, attempt_no)
        REFERENCES app.route_attempts (request_id, attempt_no)
        ON UPDATE RESTRICT
        ON DELETE RESTRICT,
    CONSTRAINT route_retry_decisions_attempt_positive CHECK (attempt_no > 0),
    CONSTRAINT route_retry_decisions_policy_version_format CHECK (
        char_length(retry_policy_version) BETWEEN 1 AND 128
        AND retry_policy_version ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]*$'
    ),
    CONSTRAINT route_retry_decisions_payload_valid CHECK (
        jsonb_typeof(retry_decision) = 'object'
        AND octet_length(retry_decision::text) <= 16384
    )
);

CREATE INDEX idx_route_decisions_selected_time
    ON app.route_decisions (selected_deployment_id, decided_at DESC, request_id)
    WHERE selected_deployment_id IS NOT NULL;

COMMENT ON TABLE app.route_decisions IS
    'Content-free routing evaluations ordered per GatewayRequest; includes filtered candidates, policy score facts, retry decision and final selection';
COMMENT ON COLUMN app.route_decisions.candidate_decisions IS
    'Bounded safe projection of deployment IDs, eligibility and finite first-failure reasons; never endpoints, provider bodies, credentials or content';
COMMENT ON COLUMN app.route_decisions.policy_decision IS
    'Bounded priority/weight/eligible-count/random-draw facts for a selected deployment';
COMMENT ON COLUMN app.route_decisions.retry_decision IS
    'Safe retry classifier Decision only; the inspected Failure and private cause are never persisted';
COMMENT ON TABLE app.route_retry_decisions IS
    'One safe retry classifier Decision for every completed physical Attempt, including terminal no-retry results';

COMMIT;
