-- Create the policy list table when it does not exist.
CREATE TABLE IF NOT EXISTS policy_list_filtered (
    id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    policy_name text,
    criticality text,
    category text,
    active text,
    "Tenant" text,
    "Device_Category" text
);

-- Add playbook usage to existing installations without changing existing rows.
ALTER TABLE policy_list_filtered
    ADD COLUMN IF NOT EXISTS playbook_usage text;

-- The importer matches policies without relying on generated ids.
CREATE INDEX IF NOT EXISTS policy_list_filtered_import_match_idx
ON policy_list_filtered (policy_name, criticality, category, "Device_Category", "Tenant");
