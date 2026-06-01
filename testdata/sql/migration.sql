-- migration with comments and semicolons in strings
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  user_id INTEGER,
  email TEXT,
  note TEXT DEFAULT 'created; with semicolon'
);

ALTER TABLE users ADD COLUMN last_login TEXT;

INSERT INTO users (user_id, email, note) VALUES (42, 'ada@example.com', 'ZREP_NEEDLE insert');

UPDATE users SET note = 'checkout; done' WHERE user_id = 42;

DELETE FROM users WHERE email = 'old@example.com';

CREATE FUNCTION audit_user() RETURNS trigger AS $$
BEGIN
  INSERT INTO audit_log(user_id, note) VALUES (NEW.user_id, 'changed; inside dollar quote');
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
