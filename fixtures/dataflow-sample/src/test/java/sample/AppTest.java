package sample;

import static org.junit.jupiter.api.Assertions.assertEquals;
import org.junit.jupiter.api.Test;

public class AppTest {
    @Test
    void summaryBuildsTagAndPrice() {
        // coupon "save" -> "T-SAVE"; amount 50 -> net 45 -> "$45"
        assertEquals("T-SAVE/$45", new App().summary("save", 50));
    }
}
