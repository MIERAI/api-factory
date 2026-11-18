-- 演示数据: 分库 shard_a(店铺 1〜2)
INSERT INTO store_prices (store_code, product_code, valid_from, valid_to, price) VALUES
  (1, 'A', '2026-10-01', '2026-10-31', 100),
  (1, 'A', '2026-11-01', NULL, 120),
  (1, 'B', '2026-11-15', NULL, 50),
  (2, 'A', '2026-11-01', NULL, 118)
ON CONFLICT DO NOTHING;
