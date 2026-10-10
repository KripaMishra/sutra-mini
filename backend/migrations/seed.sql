-- Seed data from the brief: 5 SKUs with image URLs, 2 job-workers, 4 open
-- issues. Safe to run repeatedly: SKUs upsert on code, parties and issues are
-- only written when their table is still empty.

INSERT INTO skus (code, name, image_url, rate_paise) VALUES
    ('CHK-01', 'Chikankari Kurta',  'https://picsum.photos/seed/sutra-chikankari/640/800',  8500),
    ('ZRD-02', 'Zardozi Lehenga',   'https://picsum.photos/seed/sutra-zardozi/640/800',    42000),
    ('GOT-03', 'Gota Patti Saree',  'https://picsum.photos/seed/sutra-gota/640/800',       18500),
    ('PHS-04', 'Phulkari Dupatta',  'https://picsum.photos/seed/sutra-phulkari/640/800',    9600),
    ('KDK-05', 'Kundan Blouse',     'https://picsum.photos/seed/sutra-kundan/640/800',      7400)
ON CONFLICT (code) DO NOTHING;

INSERT INTO parties (name, phone)
SELECT v.name, v.phone
FROM (VALUES
    ('Meera Embroidery Works', '+91 98110 22114'),
    ('Rahul Stitching Unit',   '+91 98110 55820')
) AS v(name, phone)
WHERE NOT EXISTS (SELECT 1 FROM parties)
ORDER BY v.name;

INSERT INTO jobwork_issues (party_id, sku_id, qty, issued_on)
SELECT p.id, s.id, v.qty, v.issued_on
FROM (VALUES
    ('Meera Embroidery Works', 'CHK-01', 120, DATE '2026-10-06'),
    ('Meera Embroidery Works', 'GOT-03', 80,  DATE '2026-10-07'),
    ('Rahul Stitching Unit',   'ZRD-02', 40,  DATE '2026-10-05'),
    ('Rahul Stitching Unit',   'KDK-05', 150, DATE '2026-10-08')
) AS v(party, code, qty, issued_on)
JOIN parties p ON p.name = v.party
JOIN skus   s ON s.code = v.code
WHERE NOT EXISTS (SELECT 1 FROM jobwork_issues)
ORDER BY v.code;
