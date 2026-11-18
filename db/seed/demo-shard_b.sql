-- 演示数据: 分库 shard_b(店铺 3〜)
INSERT INTO store_prices (store_code, product_code, valid_from, valid_to, price) VALUES
  (3, 'A', '2026-11-01', NULL, 125)
ON CONFLICT DO NOTHING;
