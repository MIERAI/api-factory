-- 演示数据(只用于 dev 的 GitOps 环境: SEED=demo)。重复执行安全(ON CONFLICT DO NOTHING)
INSERT INTO jobs (id, job_type, status, created_at, finished_at) VALUES
  (1, 'import', 1, '2026-11-01 09:00+09', '2026-11-01 09:05+09'),
  (2, 'import', 2, '2026-11-02 09:00+09', '2026-11-02 09:01+09'),
  (3, 'export', 1, '2026-11-03 10:00+09', '2026-11-03 10:20+09'),
  (4, 'export', 0, '2026-11-04 11:00+09', NULL),
  (5, 'report', 1, '2026-11-05 12:00+09', '2026-11-05 12:02+09')
ON CONFLICT DO NOTHING;

INSERT INTO daily_sales (sales_date, store_code, amount, discount, closed_on) VALUES
  ('2026-11-01', 1, 120000.50, 1500, '2026-11-02'),
  ('2026-11-01', 2,  98000,    NULL, '2026-11-02'),
  ('2026-11-01', 3, 143500.25, 2200, NULL),
  ('2026-11-02', 1, 110500,    0,    NULL)
ON CONFLICT DO NOTHING;

INSERT INTO price_changes (store_code, product_code, proc_date, new_price, old_price, deal_id) VALUES
  (1, '4901', '2026-11-20', 98,  108, NULL),
  (1, '4902', '2026-11-20', 150, 150, NULL),
  (1, '4903', '2026-11-20', 150, 150, 99999999),
  (1, '4904', '2026-11-20', 200, NULL, NULL)
ON CONFLICT DO NOTHING;

INSERT INTO product_label_events (id, data, action_type, registered_at) VALUES
  (1, '{"product_code": "100", "label_code": "A", "text": "sale"}', 0, '2026-11-01 09:00+09'),
  (2, '{"product_code": "100", "label_code": "B", "text": "new"}',  0, '2026-11-02 09:00+09'),
  (3, '{"product_code": "100", "label_code": "A", "text": "big sale"}', 0, '2026-11-03 09:00+09'),
  (4, '{"product_code": "100", "label_code": null, "text": "default"}', 0, '2026-11-03 10:00+09')
ON CONFLICT DO NOTHING;
