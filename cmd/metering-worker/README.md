# metering-worker

计量 Worker。消费至少一次投递的用量事件，幂等写入 Usage Ledger、结算预算并异步写分析存储。

