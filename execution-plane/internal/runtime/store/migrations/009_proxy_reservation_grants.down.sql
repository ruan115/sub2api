ALTER TABLE proxy_leases
    DROP FOREIGN KEY fk_proxy_leases_reservation_binding,
    DROP CHECK chk_proxy_leases_binding_revision,
    DROP CHECK chk_proxy_leases_generation,
    DROP INDEX idx_proxy_leases_reservation,
    DROP COLUMN binding_revision,
    DROP COLUMN desired_generation,
    DROP COLUMN reservation_id;

ALTER TABLE slot_assignments
    DROP CHECK chk_slot_assignments_desired_generation,
    DROP COLUMN desired_generation;

DROP TABLE proxy_reservation_grants;
