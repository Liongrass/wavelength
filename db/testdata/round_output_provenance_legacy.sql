INSERT INTO rounds (round_id, creation_time, last_update_time)
VALUES ('00000000-0000-0000-0000-000000000024', 1700000000, 1700000000);

INSERT INTO round_vtxo_requests (
    round_id, request_index, amount, pk_script, expiry,
    client_pubkey, operator_pubkey
) VALUES (
    '00000000-0000-0000-0000-000000000024', 0, 12000, X'512001', 144,
    X'020101', X'020102'
);
