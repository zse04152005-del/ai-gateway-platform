# 数据库 ER 设计草案

> 状态：Proposed  
> 日期：2026-07-30  
> 对应任务：P01-T07

## 1. ER 图

```mermaid
erDiagram
    TENANT ||--o{ PROJECT : owns
    PROJECT ||--o{ VIRTUAL_API_KEY : issues
    TENANT ||--o{ LOGICAL_MODEL : exposes
    PROVIDER ||--o{ DEPLOYMENT : operates
    LOGICAL_MODEL ||--o{ MODEL_DEPLOYMENT : maps
    DEPLOYMENT ||--o{ MODEL_DEPLOYMENT : participates
    DEPLOYMENT ||--o{ PRICE_VERSION : priced_by
    PROJECT ||--o{ ROUTE_POLICY_VERSION : configures
    PROJECT ||--o{ GATEWAY_REQUEST : sends
    VIRTUAL_API_KEY ||--o{ GATEWAY_REQUEST : authenticates
    GATEWAY_REQUEST ||--o{ ROUTE_ATTEMPT : contains
    ROUTE_ATTEMPT ||--o{ STREAM_SEGMENT : observes
    ROUTE_ATTEMPT ||--o{ USAGE_LEDGER : charges
    PRICE_VERSION ||--o{ USAGE_LEDGER : values
    PROJECT ||--o{ BUDGET_ACCOUNT : owns
    BUDGET_ACCOUNT ||--o{ BUDGET_RESERVATION : reserves
    GATEWAY_REQUEST ||--o{ BUDGET_RESERVATION : consumes
    TENANT ||--o{ SECRET_REFERENCE : stores
    TENANT ||--o{ CONFIG_VERSION : publishes
    TENANT ||--o{ AUDIT_LOG : records

    TENANT {
      uuid id PK
      text slug UK
      text name
      text status
      text quota_policy_ref
      bigint version
      timestamptz created_at
      text created_by
      timestamptz updated_at
      text updated_by
      timestamptz disabled_at
    }
    PROJECT {
      uuid id PK
      uuid tenant_id FK
      text slug
      text name
      text status
      text quota_policy_ref
      bigint version
      timestamptz created_at
      text created_by
      timestamptz updated_at
      text updated_by
      timestamptz disabled_at
    }
    VIRTUAL_API_KEY {
      uuid id PK
      uuid tenant_id FK
      uuid project_id FK
      text key_prefix UK
      bytea secret_hash
      text hash_key_version
      text status
      timestamptz expires_at
      text_array allowed_models
      jsonb limits
      uuid rotated_from_id FK
      timestamptz rotation_grace_expires_at
      timestamptz revoked_at
      text revoked_by
      bigint version
      timestamptz created_at
      text created_by
      timestamptz updated_at
      text updated_by
    }
    PROVIDER {
      uuid id PK
      text provider_type
      text name
      text status
    }
    LOGICAL_MODEL {
      uuid id PK
      uuid tenant_id FK
      text name
      jsonb required_capabilities
      text status
    }
    DEPLOYMENT {
      uuid id PK
      uuid provider_id FK
      text physical_model
      text endpoint
      text region
      jsonb capabilities
      uuid secret_reference_id FK
      text status
      bigint version
    }
    MODEL_DEPLOYMENT {
      uuid logical_model_id FK
      uuid deployment_id FK
      int priority
      int weight
    }
    PRICE_VERSION {
      uuid id PK
      uuid deployment_id FK
      text currency
      timestamptz effective_at
      jsonb unit_prices
      text status
    }
    ROUTE_POLICY_VERSION {
      uuid id PK
      uuid tenant_id FK
      uuid project_id FK
      int version
      jsonb policy
      text status
      text checksum
    }
    GATEWAY_REQUEST {
      uuid id PK
      uuid tenant_id FK
      uuid project_id FK
      uuid virtual_key_id FK
      text logical_model
      text status
      uuid route_policy_version_id FK
      timestamptz started_at
      timestamptz ended_at
    }
    ROUTE_ATTEMPT {
      uuid id PK
      uuid request_id FK
      int attempt_no
      uuid deployment_id FK
      text status
      timestamptz first_byte_at
      text end_reason
      jsonb usage_summary
    }
    STREAM_SEGMENT {
      uuid id PK
      uuid attempt_id FK
      int sequence_no
      bigint byte_count
      bigint estimated_tokens
      timestamptz observed_at
    }
    USAGE_LEDGER {
      uuid id PK
      uuid event_id UK
      uuid attempt_id FK
      text token_type
      numeric quantity
      text source
      uuid price_version_id FK
      numeric amount
      uuid adjusts_ledger_id FK
    }
    BUDGET_ACCOUNT {
      uuid id PK
      uuid tenant_id FK
      uuid project_id FK
      text scope_type
      uuid scope_id
      text currency
      numeric hard_limit
      numeric settled_amount
      numeric reserved_amount
      bigint version
    }
    BUDGET_RESERVATION {
      uuid id PK
      uuid account_id FK
      uuid request_id FK
      numeric amount
      text status
      timestamptz expires_at
      bigint version
    }
    SECRET_REFERENCE {
      uuid id PK
      uuid tenant_id FK
      text name
      bytea ciphertext
      text key_version
    }
    CONFIG_VERSION {
      uuid id PK
      uuid tenant_id FK
      bigint version
      text checksum
      text status
      timestamptz published_at
    }
    AUDIT_LOG {
      uuid id PK
      uuid tenant_id FK
      text actor_type
      text actor_id
      text action
      text resource_type
      text resource_id
      jsonb change_summary
      timestamptz occurred_at
    }
```

## 2. 关键约束

- `tenant(slug)` 全局唯一；`project(tenant_id, slug)` 唯一。
- `project(tenant_id, lower(name))` 大小写不敏感唯一。
- `project(tenant_id, id)` 唯一，供后续子表建立租户隔离复合外键。
- `virtual_api_key(key_prefix)` 全局唯一；`(hash_key_version, secret_hash)` 唯一，Hash 强制为 32 字节 keyed digest。
- `virtual_api_key(tenant_id, project_id)` 通过复合外键引用 Project，轮换自引用也必须位于同一 Tenant/Project；一个旧 Key 最多有一个替代 Key。
- `logical_model(tenant_id, name)` 唯一。
- `model_deployment(logical_model_id, deployment_id)` 唯一。
- `price_version(deployment_id, effective_at)` 唯一。
- `route_policy_version(project_id, version)` 唯一。
- `route_attempt(request_id, attempt_no)` 唯一。
- `stream_segment(attempt_id, sequence_no)` 唯一。
- `usage_ledger(event_id)` 唯一。
- 活跃预算预留可以用 `(request_id, account_id, status)` 与业务约束防重复。

## 3. 索引建议

- `gateway_request(tenant_id, project_id, started_at desc, id)`。
- `gateway_request(tenant_id, status, started_at)`。
- `route_attempt(request_id, attempt_no)`。
- `route_attempt(deployment_id, started_at, status)`。
- `usage_ledger(attempt_id, created_at)`。
- `budget_reservation(status, expires_at)` 用于 Reaper。
- `audit_log(tenant_id, occurred_at desc, id)`。

## 4. 分区与保留

- MVP 不提前分区，先通过测试数据确定规模。
- Request/Attempt/Ledger/Audit 达到量级后按月分区。
- Prompt/Response 不进入这些表。
- ClickHouse 保存查询副本，PostgreSQL 保留可审计的核心事实。

## 5. 并发策略

- 可修改配置实体使用 `version` 乐观锁。
- BudgetAccount 预留使用原子条件更新或短事务行锁。
- Ledger 通过 eventId 唯一约束实现最终幂等。
- 已发布配置/价格记录不更新，创建新版本。
