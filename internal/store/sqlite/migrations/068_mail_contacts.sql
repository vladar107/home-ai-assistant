-- The addresses each principal has sent mail to from the assistant's own mailbox, filled when a
-- send succeeds, so a proposal can mark a recipient this principal has never mailed before.
-- Addresses are kept lowercase.
CREATE TABLE mail_contacts (
    principal_id TEXT NOT NULL,
    address TEXT NOT NULL
        CHECK (length(address) BETWEEN 3 AND 320),
    first_sent_at TEXT NOT NULL,
    last_sent_at TEXT NOT NULL,
    PRIMARY KEY (principal_id, address),
    FOREIGN KEY (principal_id) REFERENCES principals (id)
        ON UPDATE CASCADE ON DELETE CASCADE
) STRICT;
