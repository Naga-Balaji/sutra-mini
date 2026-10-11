-- Seed data: 5 SKUs, 2 job-workers, 4 open issues.

INSERT INTO skus (code, name, image_url) VALUES
    ('ANK-001', 'Rose Anarkali Kurta',      'https://picsum.photos/seed/sutra-ank-001/400/500'),
    ('LEH-002', 'Zari Lehenga Skirt',       'https://picsum.photos/seed/sutra-leh-002/400/500'),
    ('SAR-003', 'Chanderi Saree',           'https://picsum.photos/seed/sutra-sar-003/400/500'),
    ('DUP-004', 'Mirror-work Dupatta',      'https://picsum.photos/seed/sutra-dup-004/400/500'),
    ('KUR-005', 'Chikankari Straight Kurta','https://picsum.photos/seed/sutra-kur-005/400/500');

INSERT INTO parties (name, phone) VALUES
    ('Meena Embroidery Works', '+91-98100-00001'),
    ('Rafiq Stitching Unit',   '+91-98100-00002');

-- rate_paise: 4500 paise = Rs 45.00 per piece, etc.
INSERT INTO jobwork_issues (party_id, sku_id, qty, issued_on, rate_paise)
SELECT p.id, s.id, v.qty, v.issued_on::date, v.rate_paise
FROM (VALUES
    ('Meena Embroidery Works', 'ANK-001', 100, '2026-10-01', 4500),
    ('Meena Embroidery Works', 'DUP-004',  60, '2026-10-02', 3000),
    ('Rafiq Stitching Unit',   'LEH-002',  40, '2026-10-03', 12000),
    ('Rafiq Stitching Unit',   'KUR-005',  80, '2026-10-04', 2500)
) AS v(party, sku, qty, issued_on, rate_paise)
JOIN parties p ON p.name = v.party
JOIN skus    s ON s.code = v.sku;
